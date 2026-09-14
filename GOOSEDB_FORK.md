# goosedb fork of pg_query_go

Fork point: upstream tag **v6.2.2** (`6a1adb4`, `pganalyze/pg_query_go`).
Branch: `stack-depth-guard`.
Licenses are unchanged and unmodified (`LICENSE`, `LICENSE.POSTGRESQL`, plus
protobuf-c's notice inside `parser/protobuf-c.c`).

## Why this fork exists

Untrusted SQL could kill the calling process. A deeply nested statement made
libpg_query recurse until the OS thread stack ran out, and the process died
with SIGSEGV/SIGBUS instead of returning an error. In Go this is unrecoverable:
the signal arrives during a cgo call, so `recover()` never sees it.

Two independent defects combine to cause it.

**1. The PostgreSQL stack-depth guard is shipped but disabled.**
`check_stack_depth()`, `stack_is_too_deep()` and `max_stack_depth` are all
present, but `stack_is_too_deep()` only reports true when `stack_base_ptr` is
non-NULL, and the body of `set_stack_base()` — the function that sets it — had
been stripped, leaving only the declaration in `miscadmin.h` and the comment in
`src_backend_tcop_postgres.c`. `assign_max_stack_depth()` was stripped the same
way, so `max_stack_depth_bytes` was stuck at its 100kB compile-time default.

**2. libpg_query's own recursive walkers never call the guard.**
Every walker inherited from PostgreSQL calls `check_stack_depth()`
(`copyfuncs.c`, `nodeFuncs.c`, `equalfuncs.c` all do). The walkers libpg_query
added — the protobuf and JSON serializers, the protobuf reader, the deparser,
the normalize walker — did not, and neither did the vendored protobuf-c runtime. So even with the guard armed,
nothing on the paths that actually overflow would have consulted it.

## Changes against v6.2.2 (complete list)

### Arming the guard

| File | Change |
|---|---|
| `parser/src_backend_tcop_postgres.c` | Restore the bodies of `set_stack_base()`, `restore_stack_base()` and `assign_max_stack_depth()`, verbatim from PostgreSQL. |
| `parser/pg_query.c` | Add `pg_query_arm_stack_guard()` and call it from `pg_query_enter_memory_context()` — the one function every public entry point already goes through. It sets the stack base and derives `max_stack_depth` from the **running thread's actual stack size** (`pthread_get_stacksize_np` on macOS, `pthread_getattr_np` on glibc), allowing the recursion half of it, clamped to [64kB, 1GB], falling back to PostgreSQL's own 2MB default when the platform will not say. It also resets protobuf-c's nesting counter. |

A fixed limit would be wrong by construction: the same statement that is safe
on an 8MB thread overflows a 512kB one, and Go moves cgo calls between OS
threads, so the limit has to be re-derived per thread. Both variables are
`__thread`, so arming per entry (rather than once) is also what keeps the base
at the outermost frame of the current call.

### Consulting the guard on the paths that overflow

| File | Function | Note |
|---|---|---|
| `parser/pg_query_outfuncs_protobuf.c` | `_outNode` | Single dispatcher every nesting level of the serializer passes through. |
| `parser/pg_query_outfuncs_json.c` | `_outNode` | Same, for the JSON serializer (`v6.2.2-goosedb.3`). |
| `parser/pg_query_readfuncs_protobuf.c` | `_readNode` | Same, for the reader. |
| `parser/postgres_deparse.c` | `deparseExpr`, `deparseStmt` | The two dispatchers; expression nesting goes through the first, statement nesting through the second. |
| `parser/pg_query_normalize.c` | `const_record_walker` | Most node types reach `raw_expression_tree_walker()`, which checks, but the `SelectStmt` case recurses into the walker directly, so a `UNION` chain never did (`v6.2.2-goosedb.5`). |
| `parser/protobuf-c.c` | `protobuf_c_message_unpack`, `protobuf_c_message_get_packed_size` | See below. |

### protobuf-c

protobuf-c upstream has **no** recursion limit at all, and its unpack path puts
a `ScannedMember` slab on the stack per invocation (~1kB per nesting level), so
it is the single most expensive walker here.

* `protobuf_c_message_unpack` is split: the body becomes
  `protobuf_c_message_unpack_bounded` and the public symbol becomes a thin
  wrapper. The wrapper is what owns the bound — the body has many early
  returns, so incrementing/decrementing in place would leak depth on some of
  them and eventually reject valid input.
* The wrapper calls `check_stack_depth()` **and** keeps a `__thread` level
  counter (`PROTOBUF_C_MAX_UNPACK_NESTING`, 10000 — the same default
  protobuf-go uses). These are not redundant: the stack check is the one that
  matters for libpg_query, but it raises through PostgreSQL's error machinery,
  which is only safe for callers whose allocations unwind with it — every unpack
  call in this tree passes the palloc allocator. The counter is an absolute
  ceiling that does not depend on the thread's stack size.
  ⚠️ This paragraph used to say the counter's NULL return is the safe rejection
  for a caller with a malloc-backed allocator. That cannot hold: the wrapper
  calls `check_stack_depth()` first, so such a caller is longjmp'ed out before
  the counter is consulted. The same wrong justification is still in the code
  comments of `parser/protobuf-c.c` (the counter definition and the wrapper);
  they were deliberately left as they are — see
  [libpg_query#366 과의 차이](#libpg_query366-과의-차이--의도적으로-옮기지-않은-것).
* `protobuf_c_message_get_packed_size` gets the guard, and `pack` deliberately
  does not. libpg_query always calls `get_packed_size()` first, then `malloc()`s
  the output buffer, then calls `pack()`. Raising from `get_packed_size()`
  therefore happens **before** any malloc, so the longjmp leaks nothing; a guard
  inside `pack()` would abandon that buffer on every rejection. `pack()` walks
  the same tree to the same depth immediately afterwards, so a tree that fits
  the first walk fits the second.

### Making the error path safe

| File | Change |
|---|---|
| `parser/pg_query.c`, `parser/pg_query_internal.h` | Add `pg_query_protobuf_allocator`, a palloc/pfree-backed `ProtobufCAllocator`. |
| `parser/pg_query_readfuncs_protobuf.c`, `parser/pg_query_deparse.c` | Pass that allocator to the two `*__unpack()` calls (and to the matching `free_unpacked()`), and raise `ERRCODE_STATEMENT_TOO_COMPLEX` when unpack returns NULL. Upstream left `Assert(result != NULL)` with a TODO; `Assert` is a no-op in release builds, so a NULL then faulted on the next dereference. |
| `parser/pg_query_parse.c` | Wrap the serialization step — protobuf and JSON (`v6.2.2-goosedb.3`) — in its own `PG_TRY`/`PG_CATCH`. `pg_query_raw_parse()` has one, but it ends before serialization runs, so an `ereport(ERROR)` raised while walking the tree had no exception stack and PostgreSQL escalated it to **FATAL** — the process exited instead of returning an error. |
| `parser/pg_query_normalize.c` | `const_record_walker`'s catch-all `PG_CATCH` re-throws `ERRCODE_STATEMENT_TOO_COMPLEX` instead of flushing it, and assigns its result instead of returning from inside `PG_TRY` (`v6.2.2-goosedb.5`). See the fix history. |

Without the palloc allocator the unpack guard is not usable at all: the longjmp
would abandon every submessage protobuf-c had malloc'ed, an unbounded leak on a
path untrusted input controls — a worse bug than the one being fixed.

## Measured effect (darwin/arm64, subprocess-isolated)

`SELECT 1+1+…` is the cheapest shape per byte (2 bytes per two protobuf nesting
levels). Before/after, same binary otherwise:

| input | v6.2.2 | this branch |
|---|---|---|
| 6KB (`chain` 3000) | SIGBUS in deparse | `stack depth limit exceeded` |
| 8KB (`chain` 4000) | SIGBUS in deparse | `stack depth limit exceeded` |
| 48KB (`chain` 24000) | SIGSEGV in protobuf-c pack | `stack depth limit exceeded` |
| 128KB (`chain` 64000) | FATAL, process exits | `stack depth limit exceeded` |
| 409KB (`chain` 204800) | SIGSEGV | `stack depth limit exceeded` |

Both output paths are covered. `pg_query_parse` (JSON) and
`pg_query_parse_protobuf` return `stack depth limit exceeded` at every depth
tested, up to 500KB of input; neither kills the process.

Control group — large but shallow input must keep working, and does:
`SELECT true OR true …` ×100000 (800KB, nesting depth 11) and
`SELECT ((((1))))` ×8000 both still return normally.

## Fix history

**`v6.2.2-goosedb.2`** — allocator mismatch in `pg_query_deparse_comments_for_query`.
The unpack was switched to the palloc-backed allocator but the matching
`free_unpacked` still passed `NULL`, so palloc'd memory was handed to the
default allocator's `free()`. Found by libpg_query's own test suite while
porting these changes upstream (`test/deparse` aborted in `mfm_free` on the
first query); pg_query_go does not expose that entry point, so no Go consumer
could reach it. A sweep of every `__unpack` / `__free_unpacked` pair in `src/`
found no other mismatch.

**`v6.2.2-goosedb.3`** — the JSON output path was still unguarded. Only the
protobuf serializer had `check_stack_depth()`, and only the protobuf entry point
had its own `PG_TRY` around serialization, so `pg_query_parse()` /
`ParseToJSON()` still died on deeply nested input (SIGSEGV, then -- once the
check was added -- a FATAL exit, because the ereport had no exception stack).
Both are fixed the same way as the protobuf path. Found while porting these
changes to libpg_query, whose primary C entry point is the JSON one; pg_query_go
exposes `ParseToJSON` too, so this was reachable from Go as well.

**`v6.2.2-goosedb.4`** — `parser/pg_query.c` did not compile with GCC 14 on
glibc. `pthread_getattr_np()` is a GNU extension that glibc declares only under
`_GNU_SOURCE`, and nothing defined it, so the call was an implicit function
declaration: a warning up to GCC 13, an error from GCC 14. macOS never takes
that branch (`__APPLE__` comes first), which is why every darwin build and test
stayed green; the first compiler to see it was the manylinux_2_28 image
(gcc-toolset-14, glibc 2.28). Fixed by defining `_GNU_SOURCE` at the top of
`pg_query.c`, before the first `#include` — `<features.h>` latches the feature
macros on the first system header, so a later define has no effect. It is kept
to that one file rather than added to the cgo `CFLAGS`: the pregenerated
`pg_config.h` sets `STRERROR_R_INT`, which assumes the XSI `strerror_r`, and a
package-wide `_GNU_SOURCE` would hand `src_port_strerror.c` the GNU prototype
instead. Verified in that image: the file compiles cleanly under `-Wall`, the
Go test suite passes, and deep input returns `stack depth limit exceeded`
instead of killing the process (`chain` 24000 on the protobuf path, `chain`
204800 on both paths) while the shallow control (`OR` ×100000) still parses.

**`v6.2.2-goosedb.5`** — `Normalize` was not safe on deep input, in three ways,
all in `const_record_walker()` in `parser/pg_query_normalize.c`. Found while
validating the upstream PR (libpg_query#366, commit `552dfe8` there); ported
from it. The consuming repository does not call `Normalize`, so no consumer
reached it, but the fork's purpose is that no entry point can.

1. *Partial result reported as success.* The walker's catch-all case wraps
   `raw_expression_tree_walker()` in a `PG_TRY` whose `PG_CATCH` flushes every
   error. It predates the guard, but once the walker could raise `stack depth
   limit exceeded` it swallowed that too, and the walk carried on from the
   next sibling. `SELECT 1+1+…` ×24000 came back with no error and only some of
   its constants replaced (measured: 10,073 placeholders out of 250,001 at
   ×250000). The catch now re-throws `ERRCODE_STATEMENT_TOO_COMPLEX`.
2. *Crash on a long `UNION` chain.* `SelectStmt` recurses into
   `const_record_walker()` directly for each clause, so a chain of set
   operations never reached the check in `raw_expression_tree_walker()`.
   `UNION ALL` ×100000 still killed the Go test process (and ×5000 did on a
   512kB thread, measured on the libpg_query tree). The walker now checks on
   entry.
3. *longjmp into a dead frame.* The same case returned from inside `PG_TRY`,
   which skips `PG_END_TRY` and leaves `PG_exception_stack` pointing at a frame
   that has returned. An error raised later — once a sibling clause had been
   walked — jumped into it. Measured with (1) and (2) fixed but this left in:
   on darwin/arm64 `SELECT 1 FROM t WHERE x = 1+1+…` grew past 60GB of RSS in
   ten seconds and `SELECT 1, 1+1+…` was killed by the OS; on linux/amd64 one
   died with SIGSEGV and the other passed a 1GB RSS cap. A single deep
   expression or a `UNION` chain did not show it. The result is now
   assigned and returned after `PG_END_TRY`.

Each fix was reverted on its own and `normalize_stack_depth_test.go` failed
each time, on darwin/arm64 and in the manylinux_2_28 image (linux/amd64, GCC
14.2). Because the regressions are a crash and an allocation loop, every case
runs in a child process under a 60s deadline and a 1GB RSS cap; cases peak
below 100MB when the walker behaves (measured on darwin/arm64). Inputs below the limit normalize exactly as
before (a wide `OR` list of 20,000 constants included), and the full Go test
suite passes on both platforms.

This is only reachable from `Normalize`/`NormalizeUtility`. The PL/pgSQL
statement walker (`stmts_walker` in `pg_query_parse_plpgsql.c`) has the same
flush-everything catch but was left alone on purpose: it looks only for
top-level `CREATE FUNCTION`/`DO` statements, which cannot sit below expression
depth, so a truncated walk cannot change its result; and its caller has no
`PG_TRY` of its own, so re-throwing there would turn the error into FATAL.

## libpg_query#366 과의 차이 — 의도적으로 옮기지 않은 것

**결정(2026-09-15): 이 fork 의 코드는 쓰는 쪽이 실제로 빌드하는 형상 — darwin/arm64 와
linux/amd64 glibc(manylinux_2_28), cgo — 에 필요한 것만 담는다.** upstream PR
[libpg_query#366](https://github.com/pganalyze/libpg_query/pull/366) 은 upstream CI 매트릭스
(MSVC·MSYS2·clang·valgrind·protobuf C++)와 musl 을 통과시키느라 이 fork 에 없는 수정을 더
들고 있다. 아래는 그 차이의 전수이고, **옮기지 않은 것은 코드가 아니라 이 표로 관리한다.**

| #366 커밋 | 내용 | 이 fork 상태 | 이 fork 에서 문제가 되지 않는 이유 | 다시 볼 때 |
|---|---|---|---|---|
| `552dfe8` | normalize 워커 3종(부분 정규화 성공 반환·`UNION` 직접 재귀·`PG_TRY` 안 `return`) | ✅ 반영 (`v6.2.2-goosedb.5`) | — | — |
| `b650b51` | `_GNU_SOURCE` (clang·GCC 14 의 `pthread_getattr_np` 암묵 선언) | ✅ 반영 (`v6.2.2-goosedb.4`, 같은 방식) | — | — |
| `b650b51` | `protobuf-c.h` 에서 `PROTOBUF_C__API` 가 `protobuf_c_message_unpack()` 이 아니라 카운터 선언에 붙는 위치 오류 | ❌ 그대로 (`parser/include/protobuf-c.h`·`parser/include/protobuf-c/protobuf-c.h` 두 사본) | 그 매크로는 `_WIN32` 이면서 `PROTOBUF_C_USE_SHARED_LIB` 일 때만 값이 있고, 그 밖에는 빈 문자열이다 | Windows 에서 protobuf-c 를 공유 라이브러리로 빌드할 때 |
| `b650b51` | `parser/pg_query.c` 의 `<pthread.h>` 무조건 include | ❌ 그대로 | 깨지는 것은 `pthread.h` 가 없는 MSVC 뿐이다. cgo 는 MSVC 를 쓰지 않고, Windows 의 MinGW 에는 winpthreads 가 있다 | MSVC 로 빌드할 때 |
| `b650b51` | Windows 에서 `GetCurrentThreadStackLimits()` 로 스택 크기 판별 | ❌ 없음 — 2MB fallback | 쓰는 쪽은 Windows 로 빌드하지 않는다. ⚠️ Windows 의 기본 스레드 스택은 실행 파일 헤더가 정하는데, MSVC 링커 기본값은 1MB 다. 그보다 작은 스택에서는 2MB 상한이 넘치기 전에 걸리지 않는다. Go 로 Windows 에서 쓸 때의 실제 거동은 **재지 않았다** | Windows 빌드를 시작하기 전에 |
| `b650b51` | Linux 판별을 glibc 에서 Linux 전반으로 넓히고, 메인 스레드는 `RLIMIT_STACK` | ❌ 없음 — `__GLIBC__` 일 때만 판별, 그 밖에는 2MB fallback | 쓰는 쪽은 glibc 로만 빌드한다. ⚠️ musl(Alpine 3.20, C 로 실측)의 기본 스레드 스택은 약 130kB 이고, 메인 스레드에서 `pthread_getattr_np()` 는 지금까지 매핑된 132kB 만 돌려준다. musl 에서 Go 로 쓸 때의 거동은 **재지 않았다** | Alpine/musl 빌드를 시작하기 전에 |
| `a72c8ff` | `pg_query_summary()` 에서 워크가 오류를 던지면 파서의 stderr 버퍼 누수 | ❌ 그대로 | 쓰는 쪽은 `Summary` 를 호출하지 않는다(0건). 거부된 호출 1건당 malloc 1건이 샌다(upstream 테스트에서 valgrind 실측: 2블록 2바이트) | `Summary` 를 쓰기 시작할 때 |
| `b650b51` | `protobuf-c.c` 의 카운터 주석 모순("malloc 할당자 호출자에게 NULL 반환이 안전") 정정 | ❌ 코드 주석 그대로 (정의부·wrapper) — 이 문서의 protobuf-c 절은 정정함 | 주석만의 문제다. 동작은 upstream 과 같다 | 그 주석을 근거로 판단하려 할 때 |
| `b650b51` | MSVC 에서 `__thread` 매핑 | 해당 없음 | cgo 는 MSVC 를 쓰지 않는다 | — |
| `a72c8ff` | protobuf C++ 직렬화기(`USE_PROTOBUF_CPP`) 가드 | 해당 없음 | 그 파일이 이 트리에 없다 | — |
| `0d59e7b` | C 회귀 테스트 `test/stack_depth` | 해당 없음 | 이 트리에는 C 테스트 체계가 없다. Go 회귀 테스트는 `normalize_stack_depth_test.go` 하나뿐이다 | — |

**#366 이 들어간 upstream 릴리스로 옮기면 위 ❌ 항목은 전부 저절로 해결된다.** 그 전에
Windows·musl·`Summary` 중 하나라도 쓰기 시작하면, 이 표의 해당 행을 먼저 옮긴다.

## Maintaining this fork

**`make update_source` erases every change in this document.** The `parser/`
directory is not source that lives here — it is a copy. The target does:

    rm -f parser/*.{c,h}
    rm -fr parser/include
    cp -a $(LIBDIR)/src/* parser/          # LIB_PG_QUERY_TAG = 17-6.2.2

So bumping `LIB_PG_QUERY_TAG` silently reverts the guard, and nothing in this
repository's tests would notice: upstream offers the same API, so everything
still compiles and passes. The failure only appears as a dead process on deep
input. After any `update_source`, re-apply the C changes listed above and
re-run the measurements in the previous section.

The consuming repository defends the same boundary from its side: it forbids
importing the upstream module path, and it asserts — via `go list -m`, not by
reading go.mod as text — that `github.com/hellower/pg_query_go/v6` is not
`replace`d, because a directory `replace` swaps the linked library while every
import line stays unchanged.

## PostgreSQL 18: fingerprint 호환성

🚨 **결정(2026-09-13): `pganalyze/pg_query_go` 가 libpg_query 18 기반이면서
[libpg_query#361](https://github.com/pganalyze/libpg_query/pull/361) 을 포함한 릴리스를
태그로 낼 때까지 libpg_query `17-6.2.2` 에 머문다. 이관할 때는 모든 fingerprint 를
`PG17_COMPAT` 으로 계산해야 한다.**

### libpg_query 18 에서 무엇이 바뀌었나

libpg_query `18.0.0`(2026-05-20)은 query ID 에 관한 PostgreSQL 커밋 `787514b30bb` 를
따랐다. SELECT/DML 문의 테이블 참조에 대해:

| | libpg_query ≤ 17 | libpg_query 18 기본값 |
|---|---|---|
| 테이블 이름 | 반영 | 별칭이 있으면 빠짐 |
| 별칭 | 무시 | 반영 |
| 스키마 이름 | 반영 | 무시 |

libpg_query#361 은 `pg_query_fingerprint_opts` 에 세 번째 인자로 `fingerprint_options`
비트마스크를 추가한다(그 함수의 API/ABI 변경). `PG_QUERY_FINGERPRINT_RANGEVAR_PG17_COMPAT`
(= `IGNORE_ALIASES | INCLUDE_SCHEMA`)이 ≤ 17 동작을 되살린다. **기본값은 PostgreSQL 18
방식 그대로이고**, 이 트리의 `parser/parser.go` 가 부르는 유일한 함수인
`pg_query_fingerprint()` 는 그 기본값에 고정돼 있다.

### 왜 지금 "PG17_COMPAT 으로 바꾸기" 를 할 수 없나

`17-6.2.2` 에는 그 옵션이 없고, 있더라도 17 에서는 아무 효과가 없다 — 17 은 이미 그 방식으로
계산한다. 18 로의 이관과 함께할 때만 의미가 있다.

### 쓰는 쪽에 미치는 영향(실측)

이 fork 를 쓰는 쪽은 클라이언트 호환 재작성 테이블(DBeaver, pgAdmin, pgJDBC, Trino, Grafana,
Power BI, Superset, pgbench)을 **하드코딩된 fingerprint** 로 찾는다 — 활성 키 124개.
빗나가도 에러가 아니다: 쿼리는 일반 경로로 가고 재작성만 조용히 멈춘다. 그쪽 테스트는
핸들러를 hex 리터럴로 찾으므로 초록으로 남는다.

방법(darwin/arm64): libpg_query 를 C 정적 라이브러리로 세 벌 빌드했다 — `17-6.2.2`,
`18.0.0`, #361 head(`bbfab39`, `18-latest` 기반). 각 키 뒤의 SQL 은 핸들러 주석과 테스트에서
복원했고, `17-6.2.2` 가 키를 정확히 재현할 때만 원문으로 인정했다. **124개 중 122개**
복원. 나머지 2개는 주석이 실제 키의 쿼리와 맞지 않아 측정하지 못했다.

| fingerprint 계산 방식 | 그대로 | 바뀜 |
|---|---|---|
| `18.0.0` 기본값 | 17 | **105** |
| #361 기본값 | 17 | **105** — 122개 모두 `18.0.0` 과 동일 |
| #361 `PG17_COMPAT` | **122** | **0** |

기본값에서도 살아남은 17개는 테이블 참조가 아예 없는 쿼리, 스키마·별칭 없는 테이블, 스키마가
*함수*에 붙은 쿼리(`pg_catalog.pg_show_all_settings()`), 그리고 TRUNCATE 다. 스키마를 붙이거나
별칭을 쓴 카탈로그 조회는 전부 바뀌었다.

주의: 복원한 SQL 은 별칭을 무시하는 *17 규칙 아래에서* 원문과 동치다. 주석의 별칭이 클라이언트가
실제로 보내는 것과 다르면, 18 기본값에서의 바뀜/그대로 분류는 달라질 수 있다. `PG17_COMPAT` 은
17 규칙을 따르므로 그 결과는 이 영향을 받지 않는다.

### upstream 상태 (2026-09-13 기준)

- libpg_query#361: 열림, 승인 1건, 미머지.
- pg_query_go 브랜치 `update-to-18-and-allow-fingerprint-opts`(`e6a9b98`, 2026-09-02):
  WIP 커밋 2개, PR 없음, `LIB_PG_QUERY_TAG = fingerprint-options` — 릴리스 태그가 아니라
  브랜치 이름이다. `FingerprintOption`, `FingerprintRangeVarPG17Compat`,
  `FingerprintWithOpts`, `FingerprintToUInt64WithOpts`, `FingerprintToHexStrWithOpts` 를
  추가하고, 옵션 없는 함수들은 `FingerprintDefault` 를 유지한다.

### 이관 체크리스트

1. upstream pg_query_go 릴리스가 **태그로** 나왔고, 그 libpg_query 태그가 #361 을 포함한다.
2. **이 fork 를 그 릴리스 태그로 rebase 한다 — `make update_source` 만으로는 안 된다.**
   `update_source` 는 복사된 C 소스·생성된 protobuf·테스트 데이터만 새로 고치고 Go
   래퍼(`pg_query.go`, `parser/parser.go`)는 건드리지 않는다. 그러면 `FingerprintWithOpts`
   와 `FingerprintOption` 이 여전히 없어 3단계가 컴파일되지 않는다. 릴리스 태그에는 두 쪽이
   다 들어 있다. 그 위에 다시 적용할 것:
   - 모듈 경로 변경 — 릴리스가 선언하는 모듈 경로로(WIP 브랜치는 아직 `…/v6`. 릴리스가 새
     메이저로 가면 쓰는 쪽의 import 경로도 함께 바뀐다);
   - 스택 깊이 가드(앞 절) — 그 릴리스에 libpg_query#366 이 들어가지 않았다면.
3. 쓰는 쪽의 fingerprint 호출을 `FingerprintWithOpts(…, FingerprintRangeVarPG17Compat)` 로
   바꾼다. 이 fork 의 기본값을 바꾸는 것보다 이쪽을 택한다: upstream 의 API 라서 fork 의 Go
   API 가 동일하게 유지된다.
4. 릴리스 자체로 키 대조를 다시 돌린다. #361 이 `PG17_COMPAT` 으로 보장하는 것은 테이블 참조
   처리뿐이다. 18 계열의 다른 fingerprint 변경 — PostgreSQL 18 파스 트리 변경이나
   libpg_query 자체의 변경(예: libpg_query#358: TransactionStmt 옵션이 이제 `BEGIN` /
   `START TRANSACTION` 에 반영된다) — 은 여전히 키를 움직일 수 있다.
5. 🚨 **그 릴리스가 libpg_query#349(protobuf-c → upb)를 포함하면 따로 확인한다.** 메인테이너가
   #366 대신 택하겠다고 한 변경이고, 2026-09-15 실측(#349 head `8f3319b` 대 `18-latest`)에서
   세 가지가 나왔다.
   - decode 깊이 기본 한도 100 이 `Deparse` 를 평범한 쿼리에서 실패시킨다 — 23컬럼 `||` CSV
     연결(783B), 스칼라 서브쿼리 16단, `1+1` 47항. 이 fork 의 현재 `Deparse` 경계는
     1,829(darwin/arm64)·2,110(linux/amd64) 항이다. 쓰는 쪽은 클라이언트 SQL 을 `Deparse`
     로 되돌리므로 한도를 설정할 수 없다면 이관할 수 없다.
   - encode 한도 `0xFFFF` 초과 시 serialize NULL 을 검사하지 않아 `pg_query_parse_protobuf`
     가 오류 없이 `len=0` 을 낸다(스택 16MB 이상에서 재현).
   - 워커 크래시(JSON·normalize·summary)는 그대로이고, upb encode 는 `18-latest` 보다 약 21%
     얕은 깊이에서 죽는다. #366 의 가드가 함께 들어가지 않았다면 이 fork 의 가드를 upb 판
     위에 다시 얹어야 한다.
   상세: [#349 코멘트](https://github.com/pganalyze/libpg_query/pull/349#issuecomment-5667112465).

## Upstreaming

Every change here is a candidate for upstream: the problem is not specific to
this caller. Any consumer that feeds untrusted SQL to the library can be killed
by it.

**The target is `pganalyze/libpg_query`, not `pganalyze/pg_query_go`.** All the
C changes live under `parser/`, which is a copy of libpg_query's `src/` (see
the previous section). Their places in libpg_query at tag `17-6.2.2`:

| this fork | libpg_query |
|---|---|
| `parser/pg_query.c`, `pg_query_parse.c`, `pg_query_deparse.c`, `pg_query_outfuncs_protobuf.c`, `pg_query_outfuncs_json.c`, `pg_query_readfuncs_protobuf.c`, `pg_query_normalize.c`, `postgres_deparse.c`, `pg_query_internal.h` | `src/` |
| `parser/src_backend_tcop_postgres.c` | `src/postgres/` |
| `parser/protobuf-c.c`, `parser/include/protobuf-c.h`, `parser/include/protobuf-c/protobuf-c.h` | `vendor/protobuf-c/` — itself vendored third-party code |

Two notes for whoever opens that PR:

- Most of the diff is *restoring PostgreSQL's own code* (`set_stack_base()`,
  `restore_stack_base()`, `assign_max_stack_depth()`) and adding
  `check_stack_depth()` calls that PostgreSQL itself makes in the equivalent
  walkers. That part should be uncontroversial.
- The novel part is deriving the limit from the *running thread's* stack.
  PostgreSQL can assume the main stack plus a `max_stack_depth` GUC; a library
  linked into an arbitrary host cannot. If upstream prefers an explicit API
  (`pg_query_set_max_stack_depth()`), the derivation here can become the
  default rather than the only behaviour.
- The protobuf-c changes touch code libpg_query itself vendors, so they are a
  separate decision: patch the vendored copy, or take it to `protobuf-c`
  upstream. `protobuf_c_message_unpack` has no recursion limit at all there.

libpg_query issue #9 (2016, closed) has a similar title but is a different bug:
`_outNode` recursed forever on `CreateForeignTableStmt` because of a struct
embedding. The class fixed here — unbounded recursion depth on input that is
deep but otherwise valid — is still open upstream.
