# ⚠️ This is a fork

**`github.com/hellower/pg_query_go`** — a fork of
[`pganalyze/pg_query_go`](https://github.com/pganalyze/pg_query_go) that makes the
parser **return an error instead of killing the process** on deeply nested SQL.
Everything else is upstream's.

```
go get github.com/hellower/pg_query_go/v6@v6.2.2-goosedb.5
```

```go
import pg_query "github.com/hellower/pg_query_go/v6"
```

> 🚨 **Upstream's README follows this section unedited — including the
> `github.com/pganalyze/...` import paths in its examples.** Substitute the path
> above when reading them. The body is deliberately left untouched: the diff
> against upstream is the only thing that tells a reader how much of this fork is
> actually ours, and rewriting import lines in prose would bury that.

Fork point: upstream tag **v6.2.2** (`6a1adb4`). Branch: `stack-depth-guard`.
Licenses are unchanged and unmodified. The canonical record — rationale, full
change list, measurements, maintenance notes — is [`GOOSEDB_FORK.md`](GOOSEDB_FORK.md);
this section summarises it.

## 🚨 PostgreSQL 18: upstream 릴리스까지 대기 — 이관 시 `PG17_COMPAT` 로 fingerprint

> **결정(2026-09-13): 이 fork 는 `pganalyze/pg_query_go` 가 libpg_query 18 기반이면서
> [libpg_query#361](https://github.com/pganalyze/libpg_query/pull/361) 을 포함한
> 릴리스를 태그로 낼 때까지 libpg_query `17-6.2.2` 에 머문다.**
> 릴리스되지 않은 18 브랜치로 rebase 하지 않는다.

**왜 중요한가: libpg_query 18 에서 fingerprint 의 의미가 바뀌었고, 깨져도 조용하다.**
PostgreSQL 18 의 query ID 변경을 따라, SELECT/DML 의 테이블 참조는 이제 **별칭**으로
계산되고(별칭이 있으면 테이블 이름은 빠진다) **스키마 이름은 무시된다.** 이 fork 를 쓰는
쪽은 클라이언트 호환 재작성 테이블을 카탈로그 조회의 **하드코딩된 fingerprint** 로 찾는데,
그 조회는 거의 전부 `FROM pg_catalog.pg_class c` 형태다. 그 키들로 실측한 결과:

| fingerprint 계산 방식 | 키 그대로 | 키 바뀜 |
|---|---|---|
| libpg_query `18.0.0` 기본값 | 17 | **105** |
| libpg_query#361 기본값 (모든 키에서 `18.0.0` 과 동일) | 17 | **105** |
| libpg_query#361 **`PG17_COMPAT`** | **122** | **0** |

키가 바뀌어도 에러가 나지 않는다. 조회가 빗나가 일반 경로로 흘러가고, 재작성만 조용히
사라진다. 키를 hex 리터럴로 직접 찾는 테스트는 초록으로 남는다.

**upstream 이 18 을 릴리스하면:**

- 🚨 **모든 fingerprint 호출에 `FingerprintRangeVarPG17Compat` 을 넘겨야 한다.** 옵션 없는
  `Fingerprint` / `FingerprintToUInt64` / `FingerprintToHexStr` 는 #361 이후에도
  PostgreSQL 18 기본값이다 — 옵션은 `…WithOpts` 변형에만 있다.
- 이 fork 의 기본값을 바꾸기보다 **쓰는 쪽**을 upstream 의 `FingerprintWithOpts` 로 바꾸는
  편을 택한다. upstream 자신의 API 이므로 이 fork 의 Go API 가 upstream 과 동일하게 유지된다.
- `PG17_COMPAT` 이 보장하는 것은 테이블 참조 처리뿐이다. 전환 전에 릴리스 자체로 키 대조를
  다시 돌린다 — 18 계열의 다른 fingerprint 변경은 그 보장 밖이다.
- 이관은 `make update_source` 가 아니라 **릴리스 태그로의 rebase** 다. 그 타깃은 복사된
  C 소스·protobuf·테스트 데이터만 새로 고치고 Go 래퍼(`pg_query.go`, `parser/parser.go`)는
  건드리지 않으므로 `…WithOpts` API 가 여전히 없다. rebase 후 모듈 경로 변경(릴리스의 모듈
  경로로 — 새 메이저일 수 있다)과 스택 깊이 가드를 다시 적용한다 —
  [Maintaining this fork](#maintaining-this-fork) 참조.

상태와 측정 방법: [`GOOSEDB_FORK.md`](GOOSEDB_FORK.md#postgresql-18-fingerprint-호환성).

## 🚨 거부 경계와 upstream 이관 위험 — 스택 상한 · libpg_query#349(upb)

### 이 fork 가 실제로 거부하는 깊이

> **상한 = 호출이 도는 OS 스레드 스택의 절반, `[64kB, 1GB]` 로 제한, 스택 크기를 알 수
> 없으면 2MB.** (`parser/pg_query.c` 의 `pg_query_arm_stack_guard()`)
> libpg_query#366 의 코드와 **동일하다** — #366 이 upstream 에 들어가 그 판으로 옮겨도 이
> 축은 바뀌지 않는다. ⚠️ 이 README 와 #366 PR 본문은 한때 상한을 `1MB` 로 잘못 적었다.
> 코드는 처음부터 `1GB` 였다.

Go 가 cgo 호출을 돌리는 스레드의 스택은 두 플랫폼 모두 **8MB** 로 보고되고(goroutine
2000개에서 전부), 따라서 상한은 **4MB** 다. 그 상태에서 `SELECT 1+1+…` 가 통과하는 최대
항 수(2026-09-15, `v6.2.2-goosedb.5`, 이분 탐색):

| | darwin/arm64 | linux/amd64 (manylinux_2_28, GCC 14.2) |
|---|---|---|
| cgo 스레드 스택 | 8MB | 8MB |
| `Parse` | 4,995 | 4,995 |
| `Parse` → `Deparse` | **1,829** | **2,110** |

- `Parse` 의 경계는 스택이 아니다. 4,996 항에서의 오류는 `proto: exceeded maximum recursion
  depth` — Go 쪽 `google.golang.org/protobuf` 의 Unmarshal 재귀 한도(10000단, 이항 연산자
  하나에 2단)다. 그래서 두 플랫폼에서 값이 같다.
- `Deparse` 의 경계가 이 fork 의 스택 상한이다. **이항 연산자 약 1,800 개(`+1` 로 약 3.7KB)가
  넘는 식 하나는 `Deparse` 에서 `stack depth limit exceeded` 로 거부된다.**
- 이것은 의도한 절충이다. 스택의 절반을 넘게 쓰는 입력은 실제로는 8MB 안에 들어가더라도
  거부한다. 대신 어떤 입력도 프로세스를 죽이지 못한다.

### 🚨 upstream 이 libpg_query#349(protobuf-c → upb)를 들이면

libpg_query 메인테이너는 #366 에 "#349 로 해결할 예정"이라고 답했다(2026-09-13). #349
(head `8f3319b`)를 `18-latest` 와 대조해 실측한 결과, **#349 는 이 fork 가 막는 크래시를
막지 못하고, 쓰는 쪽을 깨는 회귀를 새로 들여온다:**

1. **decode 깊이 한도 100 이 평범한 쿼리를 거부한다.** `Deparse` 가 23컬럼 `||` CSV 연결
   (783B), 스칼라 서브쿼리 16단, `1+1` 47항, 중첩 `CASE` 44단, `UNION ALL` 93개에서
   `could not parse protobuf` 를 낸다. 모두 `18-latest` 는 통과한다. **이 fork 를 쓰는 쪽은
   클라이언트 SQL 을 `Deparse` 로 되돌리므로, 한도가 그대로 들어오면 곧바로 쿼리 실패가
   된다** — `1+1` 기준 47 항으로, 현재 경계(1,829 항)의 약 40분의 1 이다.
2. **`pg_query_parse_protobuf` 가 에러 없이 빈 트리를 돌려준다.** encode 한도(`0xFFFF`)를
   넘으면 serialize 가 NULL 을 내는데 검사하지 않아, 16MB 이상 스택에서 `len=0`·오류 없음이
   된다. 문장 0개인 정상 결과로 보인다.
3. **크래시는 그대로다.** JSON 출력은 `18-latest` 와 정확히 같은 깊이에서 죽고(경계 동일),
   normalize·summary 도 같은 입력에서 똑같이 죽는다. protobuf encode 는 upb 인코더
   (`encode_field`)가 `18-latest` 보다 약 21% 얕은 깊이에서 죽는다.

**그 판으로 이관하기 전 확인할 것:** decode 깊이 한도를 설정할 수 있고 쓰는 쪽이 위 표의
`Deparse` 경계 이상으로 둘 수 있는가 · serialize NULL 검사가 들어갔는가 · 워커 스택 가드
(#366)가 함께 들어갔는가. 셋 중 하나라도 아니면 이 fork 의 가드를 upb 판 위에 다시 얹어야
하고, 1번은 가드로 해결되지 않는다.

측정 상세: [#349 코멘트](https://github.com/pganalyze/libpg_query/pull/349#issuecomment-5667112465) ·
[#366 코멘트](https://github.com/pganalyze/libpg_query/pull/366#issuecomment-5667114006).

## Why this fork exists

Deeply nested but otherwise valid SQL made libpg_query recurse until the OS
thread stack ran out, and the process died with SIGSEGV/SIGBUS instead of
returning an error. From Go that is unrecoverable: the signal arrives during a
cgo call, so `recover()` never sees it. One statement could take a server down.

Two independent defects combined to cause it.

**1. PostgreSQL's stack-depth guard is shipped but disabled.**
`check_stack_depth()`, `stack_is_too_deep()` and `max_stack_depth` are all
present, but the body of `set_stack_base()` — which sets the `stack_base_ptr`
that `stack_is_too_deep()` tests — had been stripped, so it stayed NULL forever
and the check was dead code. `assign_max_stack_depth()` was stripped the same
way, pinning the limit to the 100kB compile-time default.

**2. libpg_query's own recursive walkers never call the guard.** Every walker
inherited from PostgreSQL calls `check_stack_depth()` (copyfuncs.c, nodeFuncs.c,
equalfuncs.c). The walkers libpg_query adds — the protobuf serializer, the JSON
serializer, the protobuf reader, the deparser, the normalize walker — did not,
and neither did the vendored protobuf-c runtime. Fixing (1) alone changes almost
nothing; (2) is the substance.

## What differs from upstream — complete list

Only `parser/` (the C sources) and the module path differ. **No Go API is added,
removed or changed.**

| file | change |
|---|---|
| `parser/src_backend_tcop_postgres.c` | Restored `set_stack_base()`, `restore_stack_base()` and `assign_max_stack_depth()` bodies, verbatim from PostgreSQL. |
| `parser/pg_query.c` | Added `pg_query_arm_stack_guard()`, called from `pg_query_enter_memory_context()` — the chokepoint every entry point already goes through, so none of the nine entry points changed. The limit is derived from the **running thread's** stack (macOS `pthread_get_stacksize_np`, glibc `pthread_getattr_np` + `pthread_attr_getstack`): half the stack, clamped to `[64kB, 1GB]`, 2MB fallback when the platform will not say. Also defines the palloc-backed `pg_query_protobuf_allocator`. |
| `parser/pg_query_internal.h` | Declares that allocator. |
| `parser/pg_query_outfuncs_protobuf.c` | `check_stack_depth()` in `_outNode`. |
| `parser/pg_query_outfuncs_json.c` | `check_stack_depth()` in `_outNode`. |
| `parser/pg_query_readfuncs_protobuf.c` | `check_stack_depth()` in `_readNode`; unpack uses the palloc allocator; the unpack-failure `Assert` became an `ERRCODE_STATEMENT_TOO_COMPLEX` ereport so release builds return an error too; `free_unpacked()` gets the same allocator. |
| `parser/postgres_deparse.c` | `check_stack_depth()` in `deparseExpr` and `deparseStmt`. |
| `parser/pg_query_normalize.c` | `check_stack_depth()` at the entry of `const_record_walker` (its `SelectStmt` case recurses into itself directly, bypassing the check in `raw_expression_tree_walker`). Its catch-all `PG_CATCH` now re-throws `ERRCODE_STATEMENT_TOO_COMPLEX` instead of flushing it — flushing returned a partially normalized query as success — and no longer returns from inside `PG_TRY`, which left `PG_exception_stack` pointing at a dead frame. |
| `parser/pg_query_parse.c` | Serialization wrapped in its own `PG_TRY` — both the JSON and the protobuf entry point. `pg_query_raw_parse()` has one, but it ends *before* serialization, so an `ereport(ERROR)` raised while walking the tree had no exception stack and PostgreSQL escalated it to **FATAL**: the process exited instead of returning an error. That became reachable the moment the walkers started calling the guard. |
| `parser/pg_query_deparse.c` | Same `PG_TRY` / allocator treatment for the scan-result unpack. |
| `parser/protobuf-c.c` | `check_stack_depth()` plus a nesting counter (`PROTOBUF_C_MAX_UNPACK_NESTING`, 10000) on unpack — that path uses ~1kB of C stack per level, so a count-only limit is not enough. The check is in `get_packed_size`, not `pack`, because the former runs *before* the malloc, so a longjmp there leaks nothing. |
| `parser/include/protobuf-c.h`, `parser/include/protobuf-c/protobuf-c.h` | The counter's declaration. (Two copies exist in this tree; `-Iinclude` picks the first, so both are patched.) |
| `go.mod`, `pg_query.go`, `parser/build_cgo.go`, and seven `*_test.go` files | Module renamed to `github.com/hellower/pg_query_go/v6` — the `go.mod` line, the blank imports that pull `parser/include/**` into `go mod vendor`, and the import paths in the tests. **Fork-only, never upstreamable.** |
| `normalize_stack_depth_test.go` | Fork-only test for the normalize change: each case runs in a child process under a deadline and an RSS cap, because the regressions it guards against are a crash and an unbounded allocation loop. |
| `GOOSEDB_FORK.md`, `README.md` | This fork's record. `README.md` carries only this prepended section; upstream's body below it is untouched. |

## Measured effect (darwin/arm64, subprocess-isolated)

`SELECT 1+1+…` is the cheapest shape per byte. Same binary otherwise:

| input | upstream v6.2.2 | this fork |
|---|---|---|
| 6KB (`chain` 3000) | SIGBUS in deparse | `stack depth limit exceeded` |
| 48KB (`chain` 24000) | SIGSEGV in protobuf-c pack | `stack depth limit exceeded` |
| 128KB (`chain` 64000) | FATAL, process exits | `stack depth limit exceeded` |
| 500KB (`chain` 250000) | SIGSEGV | `stack depth limit exceeded` |

Both output paths are covered — `pg_query_parse` (JSON) and
`pg_query_parse_protobuf` behave identically.

**Large but shallow input keeps working**, which is the point of bounding depth
rather than length: `SELECT true OR true …` ×100000 (800KB, nesting depth 11) and
`SELECT ((((1))))` ×8000 both still parse normally.

## Tags

| tag | change |
|---|---|
| `v6.2.2-goosedb.1` | Initial: guard armed, `check_stack_depth()` in the recursive walkers and protobuf-c. |
| `v6.2.2-goosedb.2` | Allocator mismatch in `pg_query_deparse_comments_for_query` — the unpack had been switched to the palloc allocator but the matching `free_unpacked()` still passed `NULL`, handing palloc'd memory to the default allocator's `free()`. Found by libpg_query's own test suite while porting upstream. |
| `v6.2.2-goosedb.3` | The JSON output path was still unguarded — only the protobuf serializer had the check, and only the protobuf entry point had its own `PG_TRY`. |
| `v6.2.2-goosedb.4` | `parser/pg_query.c` did not compile with GCC 14 on glibc: `pthread_getattr_np()` needs `_GNU_SOURCE`, which nothing defined. |
| `v6.2.2-goosedb.5` | `Normalize` returned a partially normalized query as success on deep input (the walker's catch-all flushed the stack-depth error), a long `UNION` chain still killed the process, and an error after a completed sibling clause longjmp'ed into a dead frame. Found while validating libpg_query#366; ported from its commit `ee79548`. |

## Upstream contribution — pganalyze/libpg_query#366

**All C changes here have been submitted upstream.** The problem is not specific
to this caller: any consumer that feeds untrusted SQL to the library can be
killed by it.

**PR: [pganalyze/libpg_query#366](https://github.com/pganalyze/libpg_query/pull/366)**
— *"Guard libpg_query's own recursive walkers against stack exhaustion"*

- **The target repository is `libpg_query`, not `pg_query_go`.** Everything under
  `parser/` here is a *copy*: `make update_source` deletes it and re-copies from
  `libpg_query-$(LIB_PG_QUERY_TAG)/src/`. The C fix has no home in this
  repository.
- **Target branch is `18-latest`** (upstream's default), whereas this fork is
  based on `17-6.2.2`. Ten of the eleven files applied cleanly across that gap.
  The eleventh moved: PostgreSQL 18 split the stack machinery out of
  `src_backend_tcop_postgres.c` into
  `src/postgres/src_backend_utils_misc_stack_depth.c`, and it was re-applied
  there by hand.
- **Both defects were re-confirmed on `18-latest` before submitting** — not
  assumed. `check_stack_depth()` call sites were counted in the pristine tree
  (1 each in the three walkers inherited from PostgreSQL, **0** in all five of
  libpg_query's own and vendored ones), `set_stack_base()` was verified to still
  be an empty `#ifdef` shell, and the SIGSEGV was reproduced.
- **Not submitted:** the module rename and `GOOSEDB_FORK.md` — fork-only.
- **Verification in that PR:** `make build` clean, `make test` passing unchanged
  (8 / 414 / 14 assertions across the suites), and a before/after reproducer
  showing 128KB and 500KB inputs going from SIGSEGV to
  `stack depth limit exceeded`.
- **Two points left open for the maintainers**, both flagged in the PR body:
  deriving the limit from the thread's stack could instead become the default
  behind an explicit `pg_query_set_max_stack_depth()` API; and the protobuf-c
  changes touch vendored third-party code, so they may belong in a patch file or
  in protobuf-c upstream rather than in the vendored copy.
- Upstream issue [#9](https://github.com/pganalyze/libpg_query/issues/9) (2016,
  closed) has a similar title but is a **different** bug — `_outNode` recursing
  forever on `CreateForeignTableStmt` because of a struct embedding. The class
  fixed here is unbounded depth on input that is deep but otherwise valid.
- There is no `SECURITY.md` or private reporting channel on the upstream
  repository, so this was reported together with its fix rather than separately.

- **Status (2026-09-15):** the maintainer replied that the plan is to switch to
  upb instead ([#349](https://github.com/pganalyze/libpg_query/pull/349)). It was
  measured against this fix and does not cover it — see the 🚨 section at the
  top. #366 now also carries the normalize fix (`ee79548`) and offers to rebase
  onto #349, dropping the protobuf-c part.

**If that PR is merged, this fork should be retired** in favour of the upstream
release that carries it — after checking the three conditions in the 🚨 section
at the top if that release also carries #349.

## Maintaining this fork

🚨 **`make update_source` erases every change listed above.** `parser/` is a
copy, and that target runs `rm -f parser/*.{c,h}` before re-copying from
libpg_query. Bumping `LIB_PG_QUERY_TAG` silently reverts the guard, and nothing
in the test suite notices — upstream offers the same API, so everything still
compiles and passes. The failure only reappears as a dead process on deep input.
After any `update_source`, re-apply the C changes and re-run the measurements
above.

The consuming repository defends the same boundary from its side: it forbids
importing the upstream module path, and it asserts — via `go list -m`, not by
reading `go.mod` as text — that this module is not `replace`d, because a
directory `replace` swaps the linked library while every import line stays
unchanged.

---

# pg_query_go [![GoDoc](https://godoc.org/github.com/pganalyze/pg_query_go/v6?status.svg)](https://godoc.org/github.com/pganalyze/pg_query_go/v6)

Go version of https://github.com/pganalyze/pg_query

This Go library and its cgo extension use the actual PostgreSQL server source to parse SQL queries and return the internal PostgreSQL parse tree.

You can find further background to why a query's parse tree is useful here: https://pganalyze.com/blog/parse-postgresql-queries-in-ruby.html


## Installation

```
go get github.com/pganalyze/pg_query_go/v6@latest
```

Due to compiling parts of PostgreSQL, the first time you build against this library it will take a bit longer.

Expect up to 3 minutes. You can use `go build -x` to see the progress.

## Usage

### Parsing a query into JSON

Put the following in a new Go package, after having installed pg_query as above:

```go
package main

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func main() {
	tree, err := pg_query.ParseToJSON("SELECT 1")
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", tree)
}
```

Running will output the query's parse tree as JSON:

```json
{"version":170004,"stmts":[{"stmt":{"SelectStmt":{"targetList":[{"ResTarget":{"val":{"A_Const":{"ival":{"ival":1},"location":7}},"location":7}}],"limitOption":"LIMIT_OPTION_DEFAULT","op":"SETOP_NONE"}}}]}
```

### Parsing a query into Go structs

When working with the query information inside Go its recommended you use the `Parse()` method which returns Go structs:

```go
package main

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func main() {
	result, err := pg_query.Parse("SELECT 42")
	if err != nil {
		panic(err)
	}

	// This will output "42"
	fmt.Printf("%d\n", result.Stmts[0].Stmt.GetSelectStmt().GetTargetList()[0].GetResTarget().GetVal().GetAConst().GetIval().Ival)
}
```

You can find all the node types in the `pg_query.pb.go` Protobuf definition.

### Deparsing a parse tree back into a SQL statement

In order to go back from a parse tree to a SQL statement, you can use the deparsing functionality:

```go
package main

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func main() {
	result, err := pg_query.Parse("SELECT 42")
	if err != nil {
		panic(err)
	}

	result.Stmts[0].Stmt.GetSelectStmt().GetTargetList()[0].GetResTarget().Val = pg_query.MakeAConstStrNode("Hello World", -1)

	stmt, err := pg_query.Deparse(result)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", stmt)
}
```

This will output the following:

```
SELECT 'Hello World'
```

Note that it is currently not recommended to pass unsanitized input to the deparser, as it may lead to crashes.

### Parsing a PL/pgSQL function into JSON (Experimental)

Put the following in a new Go package, after having installed pg_query as above:

```go
package main

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func main() {
	tree, err := pg_query.ParsePlPgSqlToJSON(
		`CREATE OR REPLACE FUNCTION cs_fmt_browser_version(v_name varchar, v_version varchar)
  			RETURNS varchar AS $$
  			BEGIN
  			    IF v_version IS NULL THEN
  			        RETURN v_name;
  			    END IF;
  			    RETURN v_name || '/' || v_version;
  			END;
  			$$ LANGUAGE plpgsql;`)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", tree)
}
```

Running will output the functions's parse tree as JSON:

```json
$ go run main.go
[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"v_name","datatype":{"PLpgSQL_type":{"typname":"UNKNOWN"}}}},{"PLpgSQL_var":{"refname":"v_version","datatype":{"PLpgSQL_type":{"typname":"UNKNOWN"}}}},{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"UNKNOWN"}}}}],"action":{"PLpgSQL_stmt_block":{"lineno":2,"body":[{"PLpgSQL_stmt_if":{"lineno":3,"cond":{"PLpgSQL_expr":{"query":"v_version IS NULL"}},"then_body":[{"PLpgSQL_stmt_return":{"lineno":4,"expr":{"PLpgSQL_expr":{"query":"v_name"}}}}]}},{"PLpgSQL_stmt_return":{"lineno":6,"expr":{"PLpgSQL_expr":{"query":"v_name || '/' || v_version"}}}}]}}}}
]
```

## Benchmarks

```
$ make benchmark
go build -a
go test -test.bench=. -test.run=XXX -test.benchtime 10s -test.benchmem -test.cpu=4
goos: darwin
goarch: arm64
pkg: github.com/pganalyze/pg_query_go/v6
BenchmarkParseSelect1-4                          2874156              4186 ns/op            1040 B/op         18 allocs/op
BenchmarkParseSelect2-4                           824781             14572 ns/op            2832 B/op         57 allocs/op
BenchmarkParseCreateTable-4                       351037             34591 ns/op            8480 B/op        149 allocs/op
BenchmarkParseSelect1Parallel-4                  9027080              1320 ns/op            1040 B/op         18 allocs/op
BenchmarkParseSelect2Parallel-4                  2745390              4369 ns/op            2832 B/op         57 allocs/op
BenchmarkParseCreateTableParallel-4              1000000             10487 ns/op            8480 B/op        149 allocs/op
BenchmarkRawParseSelect1-4                       3778771              3183 ns/op             128 B/op          3 allocs/op
BenchmarkRawParseSelect2-4                       1000000             10985 ns/op             288 B/op          3 allocs/op
BenchmarkRawParseCreateTable-4                    460714             26397 ns/op            1056 B/op          3 allocs/op
BenchmarkRawParseSelect1Parallel-4              13338790               902.7 ns/op           128 B/op          3 allocs/op
BenchmarkRawParseSelect2Parallel-4               4060762              2956 ns/op             288 B/op          3 allocs/op
BenchmarkRawParseCreateTableParallel-4           1709883              7001 ns/op            1056 B/op          3 allocs/op
BenchmarkFingerprintSelect1-4                    6394882              1875 ns/op              48 B/op          2 allocs/op
BenchmarkFingerprintSelect2-4                    2865390              4174 ns/op              48 B/op          2 allocs/op
BenchmarkFingerprintCreateTable-4                1688920              7143 ns/op              48 B/op          2 allocs/op
BenchmarkNormalizeSelect1-4                     10604962              1133 ns/op              32 B/op          2 allocs/op
BenchmarkNormalizeSelect2-4                      6226136              1938 ns/op              64 B/op          2 allocs/op
BenchmarkNormalizeCreateTable-4                  4542387              2635 ns/op             144 B/op          2 allocs/op
PASS
ok      github.com/pganalyze/pg_query_go/v6     258.376s

```

Note that allocation counts exclude the cgo portion, so they are higher than shown here.

See `benchmark_test.go` for details on the benchmarks.


## Authors

- [Lukas Fittl](mailto:lukas@fittl.com)


## License

Copyright (c) 2015, Lukas Fittl <lukas@fittl.com><br>
Copyright (c) 2016-2025, Duboce Labs, Inc. (pganalyze) <team@pganalyze.com>
pg_query_go is licensed under the 3-clause BSD license, see LICENSE file for details.

This project includes code derived from the [PostgreSQL project](http://www.postgresql.org/),
see LICENSE.POSTGRESQL for details.
