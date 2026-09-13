# ⚠️ This is a fork

**`github.com/hellower/pg_query_go`** — a fork of
[`pganalyze/pg_query_go`](https://github.com/pganalyze/pg_query_go) that makes the
parser **return an error instead of killing the process** on deeply nested SQL.
Everything else is upstream's.

```
go get github.com/hellower/pg_query_go/v6@v6.2.2-goosedb.3
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

## 🚨 PostgreSQL 18: wait for upstream's release — then fingerprint with `PG17_COMPAT`

> **Decision (2026-09-13): this fork stays on libpg_query `17-6.2.2` until
> `pganalyze/pg_query_go` publishes a tagged release built on libpg_query 18
> that includes [libpg_query#361](https://github.com/pganalyze/libpg_query/pull/361).**
> Do not rebase onto an unreleased 18 branch.

**Why it matters: libpg_query 18 changed what a fingerprint means, and the
breakage is silent.** Following PostgreSQL 18's query ID change, relation
references in SELECT/DML are now fingerprinted by **alias** (the relation name
is dropped when an alias exists) and **schema names are ignored**. The
consumer keys a table of client-compatibility rewrites on hard-coded
fingerprints of catalog queries — nearly all of the form
`FROM pg_catalog.pg_class c`. Measured against those keys:

| fingerprint computed with | keys unchanged | keys changed |
|---|---|---|
| libpg_query `18.0.0`, default | 17 | **105** |
| libpg_query#361, default (identical to `18.0.0` on every key) | 17 | **105** |
| libpg_query#361, **`PG17_COMPAT`** | **122** | **0** |

A changed key is not an error: the lookup misses, the query falls through to
the generic path, and the rewrite simply stops happening. Tests that look keys
up by their hex literal stay green.

**When upstream releases 18:**

- 🚨 **Every fingerprint call must pass `FingerprintRangeVarPG17Compat`.** The
  plain `Fingerprint` / `FingerprintToUInt64` / `FingerprintToHexStr` keep the
  PostgreSQL 18 default even after #361 — the option only exists on the
  `…WithOpts` variants.
- Prefer switching the **consumer** to upstream's `FingerprintWithOpts` over
  changing the default here: that API is upstream's own, so this fork's Go API
  stays identical to upstream's.
- `PG17_COMPAT` is only promised for relation-reference handling. Re-run the
  key comparison on the release itself before switching — other fingerprint
  changes on the 18 line are not covered by that promise.
- The upgrade is a **rebase onto the release tag**, not a `make update_source`:
  that target refreshes only the copied C sources, protobuf and test data, and
  never touches the Go wrappers (`pg_query.go`, `parser/parser.go`), so the
  `…WithOpts` API would still be missing. After the rebase, re-apply the module
  rename (to the release's module path, which may be a new major) and the
  stack-depth guard — see [Maintaining this fork](#maintaining-this-fork).

Status and method: [`GOOSEDB_FORK.md`](GOOSEDB_FORK.md#postgresql-18-fingerprint-compatibility).

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
serializer, the protobuf reader, the deparser — did not, and neither did the
vendored protobuf-c runtime. Fixing (1) alone changes almost nothing; (2) is the
substance.

## What differs from upstream — complete list

Only `parser/` (the C sources) and the module path differ. **No Go API is added,
removed or changed.**

| file | change |
|---|---|
| `parser/src_backend_tcop_postgres.c` | Restored `set_stack_base()`, `restore_stack_base()` and `assign_max_stack_depth()` bodies, verbatim from PostgreSQL. |
| `parser/pg_query.c` | Added `pg_query_arm_stack_guard()`, called from `pg_query_enter_memory_context()` — the chokepoint every entry point already goes through, so none of the nine entry points changed. The limit is derived from the **running thread's** stack (macOS `pthread_get_stacksize_np`, glibc `pthread_getattr_np` + `pthread_attr_getstack`): half the stack, clamped to `[64kB, 1MB]`, conservative fallback otherwise. Also defines the palloc-backed `pg_query_protobuf_allocator`. |
| `parser/pg_query_internal.h` | Declares that allocator. |
| `parser/pg_query_outfuncs_protobuf.c` | `check_stack_depth()` in `_outNode`. |
| `parser/pg_query_outfuncs_json.c` | `check_stack_depth()` in `_outNode`. |
| `parser/pg_query_readfuncs_protobuf.c` | `check_stack_depth()` in `_readNode`; unpack uses the palloc allocator; the unpack-failure `Assert` became an `ERRCODE_STATEMENT_TOO_COMPLEX` ereport so release builds return an error too; `free_unpacked()` gets the same allocator. |
| `parser/postgres_deparse.c` | `check_stack_depth()` in `deparseExpr` and `deparseStmt`. |
| `parser/pg_query_parse.c` | Serialization wrapped in its own `PG_TRY` — both the JSON and the protobuf entry point. `pg_query_raw_parse()` has one, but it ends *before* serialization, so an `ereport(ERROR)` raised while walking the tree had no exception stack and PostgreSQL escalated it to **FATAL**: the process exited instead of returning an error. That became reachable the moment the walkers started calling the guard. |
| `parser/pg_query_deparse.c` | Same `PG_TRY` / allocator treatment for the scan-result unpack. |
| `parser/protobuf-c.c` | `check_stack_depth()` plus a nesting counter (`PROTOBUF_C_MAX_UNPACK_NESTING`, 10000) on unpack — that path uses ~1kB of C stack per level, so a count-only limit is not enough. The check is in `get_packed_size`, not `pack`, because the former runs *before* the malloc, so a longjmp there leaks nothing. |
| `parser/include/protobuf-c.h`, `parser/include/protobuf-c/protobuf-c.h` | The counter's declaration. (Two copies exist in this tree; `-Iinclude` picks the first, so both are patched.) |
| `go.mod`, `pg_query.go`, `parser/build_cgo.go`, and seven `*_test.go` files | Module renamed to `github.com/hellower/pg_query_go/v6` — the `go.mod` line, the blank imports that pull `parser/include/**` into `go mod vendor`, and the import paths in the tests. **Fork-only, never upstreamable.** |
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

**If that PR is merged, this fork should be retired** in favour of the upstream
release that carries it.

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
