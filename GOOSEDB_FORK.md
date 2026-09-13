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

## PostgreSQL 18: fingerprint compatibility

🚨 **Decision (2026-09-13): stay on libpg_query `17-6.2.2` until
`pganalyze/pg_query_go` tags a release built on libpg_query 18 that includes
[libpg_query#361](https://github.com/pganalyze/libpg_query/pull/361). When
moving, every fingerprint must be computed with `PG17_COMPAT`.**

### What changed in libpg_query 18

libpg_query `18.0.0` (2026-05-20) followed PostgreSQL commit `787514b30bb`
for query IDs. For relation references in SELECT/DML statements:

| | libpg_query ≤ 17 | libpg_query 18 default |
|---|---|---|
| relation name | fingerprinted | dropped when an alias is present |
| alias | ignored | fingerprinted |
| schema name | fingerprinted | ignored |

libpg_query#361 adds a `fingerprint_options` bitmask as a third argument to
`pg_query_fingerprint_opts` (an API/ABI break for that function).
`PG_QUERY_FINGERPRINT_RANGEVAR_PG17_COMPAT` (= `IGNORE_ALIASES |
INCLUDE_SCHEMA`) restores the ≤ 17 behaviour. **The default stays PostgreSQL
18's**, and `pg_query_fingerprint()` — the only function this tree's
`parser/parser.go` calls — is hard-wired to that default.

### Why this fork cannot simply "switch to PG17_COMPAT" now

The option does not exist in `17-6.2.2`, and on 17 it would be a no-op anyway:
17 already fingerprints this way. It becomes meaningful only together with the
move to 18.

### Measured impact on the consumer

The consumer keeps a table of client-compatibility rewrites (DBeaver, pgAdmin,
pgJDBC, Trino, Grafana, Power BI, Superset, pgbench) keyed on **hard-coded
fingerprints** — 124 live keys. A miss is not an error: the query takes the
generic path and the rewrite silently stops. Its tests look handlers up by hex
literal, so they would stay green.

Method (darwin/arm64): libpg_query built as a static C library three times —
`17-6.2.2`, `18.0.0`, and the #361 head (`bbfab39`, on `18-latest`). The SQL
behind each key was recovered from the handler's comment and the tests, and
accepted only if `17-6.2.2` reproduces the key exactly. **122 of 124**
recovered; the other two have comments that do not match the query they were
keyed on, and are unmeasured.

| fingerprint computed with | unchanged | changed |
|---|---|---|
| `18.0.0`, default | 17 | **105** |
| #361, default | 17 | **105** — identical to `18.0.0` on all 122 |
| #361, `PG17_COMPAT` | **122** | **0** |

The 17 that survive the default are queries with no relation at all, bare
unqualified tables with no alias, schema-qualified *functions*
(`pg_catalog.pg_show_all_settings()`), and TRUNCATE. Every schema-qualified or
aliased catalog query changed.

Caveat: recovered SQL is equivalent to the original *under 17's rules*, which
ignore aliases. If a comment's alias differs from what the client actually
sends, that key's changed/unchanged classification under the 18 default could
differ. `PG17_COMPAT` follows 17's rules, so its result does not depend on this.

### Upstream status (as of 2026-09-13)

- libpg_query#361: open, approved once, not merged.
- pg_query_go branch `update-to-18-and-allow-fingerprint-opts` (`e6a9b98`,
  2026-09-02): two WIP commits, no PR, `LIB_PG_QUERY_TAG = fingerprint-options`
  — a branch name, not a release tag. It adds `FingerprintOption`,
  `FingerprintRangeVarPG17Compat`, `FingerprintWithOpts`,
  `FingerprintToUInt64WithOpts`, `FingerprintToHexStrWithOpts`; the plain
  functions keep `FingerprintDefault`.

### Checklist for the move

1. The upstream pg_query_go release is **tagged** and its libpg_query tag
   contains #361.
2. **Rebase this fork onto that release tag — not just `make update_source`.**
   `update_source` refreshes only the copied C sources, the generated protobuf
   and test data; it never touches the Go wrappers (`pg_query.go`,
   `parser/parser.go`), so `FingerprintWithOpts` and `FingerprintOption` would
   still be missing and step 3 would not compile. The release tag carries both
   halves. On top of it, re-apply:
   - the module rename, to whatever module path the release declares (the WIP
     branch still says `…/v6`; if the release moves to a new major, the
     consumer's import paths change with it);
   - the stack-depth guard (previous section) — unless libpg_query#366 has
     landed in that release.
3. Switch the consumer's fingerprint calls to `FingerprintWithOpts(…,
   FingerprintRangeVarPG17Compat)`. Prefer this over changing the default in
   this fork: the API is upstream's, so the fork's Go API stays identical.
4. Re-run the key comparison on the release itself. #361 promises
   `PG17_COMPAT` only for relation-reference handling; other fingerprint
   changes on the 18 line — PostgreSQL 18 parse-tree changes, or libpg_query's
   own (e.g. libpg_query#358: TransactionStmt options now affect `BEGIN` /
   `START TRANSACTION`) — can still move keys.

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
