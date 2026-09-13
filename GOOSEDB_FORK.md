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
added — the protobuf serializer, the protobuf reader, the deparser — did not,
and neither did the vendored protobuf-c runtime. So even with the guard armed,
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
| `parser/pg_query_readfuncs_protobuf.c` | `_readNode` | Same, for the reader. |
| `parser/postgres_deparse.c` | `deparseExpr`, `deparseStmt` | The two dispatchers; expression nesting goes through the first, statement nesting through the second. |
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
  which is only safe for callers whose allocations unwind with it. The counter's
  NULL return remains the safe rejection for a caller that passes its own
  malloc-backed allocator, which protobuf-c's public API allows.
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
| `parser/pg_query_parse.c` | Wrap the protobuf serialization step in its own `PG_TRY`/`PG_CATCH`. `pg_query_raw_parse()` has one, but it ends before serialization runs, so an `ereport(ERROR)` raised while walking the tree had no exception stack and PostgreSQL escalated it to **FATAL** — the process exited instead of returning an error. |

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

## Upstreaming

Every change here is a candidate for upstream: the problem is not specific to
this caller. Any consumer that feeds untrusted SQL to the library can be killed
by it.

**The target is `pganalyze/libpg_query`, not `pganalyze/pg_query_go`.** All the
C changes live under `parser/`, which is a copy of libpg_query's `src/` (see
the previous section). Their places in libpg_query at tag `17-6.2.2`:

| this fork | libpg_query |
|---|---|
| `parser/pg_query.c`, `pg_query_parse.c`, `pg_query_deparse.c`, `pg_query_outfuncs_protobuf.c`, `pg_query_readfuncs_protobuf.c`, `postgres_deparse.c`, `pg_query_internal.h` | `src/` |
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
