# caddysqlite — Agent Guide

A Caddy v2 module that embeds SQLite directly into the web server. Plain Go 1.22. No code generation, no external backend. One binary, one Caddyfile, one database.

## Language & Build

- **Language**: Go 1.22 (standard `func`/`type`/`chan`/`defer`/`<-`). The `.go` files are ordinary Go; there is no Go+ or V transpilation step.
- **Build**: `go build ./...` for the library, or `xcaddy build --with caddysqlite=. --output /out/caddy` for the full Caddy binary (see `Dockerfile`).
- **No lint config, no formatter config.** `gofmt` and `go vet` are expected.

## Tests

**Run**: `go test ./...`

- Tests live next to the code they test: `pkg/database/*_test.go`, `pkg/cache/*_test.go`, `pkg/plugin/*_test.go`. No root-level tests.
- Test functions: `func TestXxx(t *testing.T)` — signal failures with `t.Errorf(msg, ...)`.
- Tests use the internal package name (`package database`, not `package database_test`) so they can reach unexported helpers (`pragmaName`, `stripComments`, `hasSideEffectingUDF`).

| File | Covers |
|---|---|
| `pkg/database/classify_test.go` | SELECT/INSERT/UPDATE/DELETE/REPLACE/UPSERT, RETURNING detection, PRAGMA read vs write vs side-effect, WITH with/without DML, comment stripping, `http_post` UDF de-caching |
| `pkg/cache/lru_test.go` | put/get, miss, expiry, overwrite, eviction, LRU ordering |
| `pkg/cache/key_test.go` | FNV64a key stability, gen/sql/arg sensitivity, type sensitivity |
| `pkg/database/functions_test.go` | `http_get` / `http_post` against `httptest` servers: body passthrough, status-code ignored, transport failure → NULL, content type default and override, arity rejection, the 1 MiB cap, and UDF visibility across pooled connections |
| `pkg/plugin/bind_test.go` | placeholder validation: known vs unknown placeholders, JSON bodies surviving a raw `{http.request.body}` bind |
| `pkg/plugin/db_test.go` | `expandEnv` set/missing/literal paths, `backup.method` accepted and rejected |
| `pkg/database/backup_test.go` | `BackupScheduler.RunOnce`: the artifact is a valid SQLite file with the live rows in it, retention prunes to the N newest, `method`/`schedule`/`dir` validation |
| `pkg/database/batch_test.go` | group commit (concurrent writes share one commit, observed via the generation counter), `RETURNING` through the batcher, size-based flush, statement error vs commit error, closed batcher, context cancellation, the no-batcher bypass |
| `pkg/database/pool_test.go` | cache hit/`gen` invalidation, the >100-row no-cache rule, single-flight (one execution for N concurrent misses, counted by a test UDF), `ScanRows` value normalization, pool refcounting and reopen |
| `pkg/database/migrate_test.go` | version ordering, non-numeric files ignored, idempotent re-apply, applying only new files, rollback of a failed migration (`user_version` unchanged, partial work gone) |
| `pkg/database/store_test.go` | `buildDSN` defaults, read-only and `:memory:` variants, pragma values on a live connection, read-only write rejection, `:memory:` visible to a second connection |
| `pkg/plugin/bg_test.go` | dispatch returns 202 + job id, result stored only with a webhook, POST and GET webhook shapes, failure status, unresolved bind → 400, 503 after cleanup, result TTL sweep |
| `pkg/plugin/dynamic_test.go` | `dynamic` column mapping (out-of-order columns), 0/many rows, unknown/duplicate columns, empty content type, empty error body, `writeFail` per format, CSV/plain/XML row layout, unknown format |
| `pkg/plugin/template_test.go` | `{{ sqlite }}` reads, write path returning an empty slice and invalidating cached reads, `RETURNING` writes, unknown DB/unclassifiable SQL errors, `runOptimize` stop signal |

**End-to-end**: `test/e2e.sh` runs the built image in Docker and exercises the HTTP surface — every output format (including `dynamic` with columns out of order), both UDFs, `sqlite_bg` with and without a webhook, a `read_only` DB, the `{{ sqlite }}` template function, a scheduled backup (`@every 1s`, checked for file creation, SQLite header, committed rows, and retention), and seven provision-time validation failures via `caddy validate`. It needs an image named `caddysqlite-e2e` (`build.sh` tags one, or pass `E2E_IMAGE`), publishes port 9099, and prints `PASS/FAIL` with a non-zero exit on failure. It is not part of `go test ./...`.

**Two tests act as regression guards** for bugs that have since been fixed:

- `TestClassifyPragmaLowercaseSideEffect` — `pkg/database/classify.go:pragmaName` strips the `PRAGMA` keyword case-insensitively, so `pragma wal_checkpoint` (lowercase) is still classified as a side-effecting write.
- `TestKeyArgsBoundary` — `pkg/cache/key.go:Key` writes `%T:%v\x00` per arg, so `["a","b"]` and `["astring:b"]` cannot collide.

They no longer `t.Skip`; keep them passing or the bug is back.

**Gotcha**: `string(int)` converts to a Unicode rune, not decimal digits. Use `fmt.Sprintf("%d", n)`.

## Project Structure

```
sqlite.go                    # Module registration + UDF registration (init)
pkg/
  plugin/                    # Caddy plugin layer — the only package that imports caddy
    db.go                    # sqlite_db directive parser, SQLiteDBs (Provision/Cleanup)
    handler.go               # sqlite_response directive parser, SQLiteResponse
    bg.go                    # sqlite_bg directive — async background queries + webhook
    bind.go                  # Bind resolution (positional vs named, shared by handler+bg)
    output.go                # writeRows/writeResult/writeFail dispatch
    response.go              # JSON, XML, CSV, plain row serializers
    dynamic.go               # output=dynamic handler
    template.go              # {{ sqlite }} template function
  database/                  # Core DB operations — no Caddy imports
    pool.go                  # Pool, DB struct, QueryCached, WriteStmt, ScanRows
    store.go                 # OpenDB (DSN, pragmas, connection pool config)
    batch.go                 # Batcher (group-commit write batching)
    exec.go                  # Batcher.commitBatch / execOne
    classify.go              # SQL classification (Plan)
    migrate.go               # Versioned schema migration via PRAGMA user_version
    backup.go                # Scheduled VACUUM INTO backup + retention
    functions.go             # http_get / http_post SQLite UDFs (SSRF-capable, see below)
  cache/                     # Per-database shared cache — no Caddy imports
    lru.go                   # LRU with per-entry TTL
    key.go                   # FNV64a key: (gen, sqlText, args)
test/
  e2e.sh                     # Docker end-to-end suite (see Tests above)
www/                         # Demo image: Caddyfile, index.html, migrations
```

`pkg/database` and `pkg/cache` have **zero** Caddy imports. The engine is testable and reusable without a Caddy context.

## SQLite User-Defined Functions (UDFs)

Registered **once, globally**, at process init via `pkg/database/functions.go:RegisterHTTPFuncs()`, which calls `sqlite.RegisterScalarFunction` from `modernc.org/sqlite`. They are available in **every** SQL query on **every** database connection opened by this module, with no Caddyfile configuration.

Registration **must** happen before any `sql.DB` is opened — the driver resolves UDFs at `sqlite3_open` time. `sqlite.go:init` calls `database.RegisterHTTPFuncs()` before `caddy.Run()` provisions any DB, so this ordering is guaranteed. Do not move the call out of `init()`.

### `http_get(url)` → TEXT | NULL

```sql
SELECT http_get('https://api.example.com/status');
```

GET request. Returns the response body as TEXT (status code ignored), or NULL on transport failure (bad URL, timeout, connection refused, read error).

### `http_post(url, body[, content_type])` → TEXT | NULL

```sql
SELECT http_post('https://hooks.example.com/notify', '{"hello":"world"}');
SELECT http_post('https://x/y', 'a=1', 'application/x-www-form-urlencoded');
```

POST request. `content_type` defaults to `application/json`. Returns the response body as TEXT, or NULL on transport failure.

### Implementation contract

```go
func RegisterHTTPFuncs() error
func httpGetFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error)
func httpPostFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error)
```

The callback signature is **`func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error)`** — `modernc.org/sqlite` (imported as `sqlite`, not `sqlite3`). Return the result as the first value; there is no result context:

- `return string(s), nil` — a TEXT value
- `return nil, nil` — NULL
- `return nil, fmt.Errorf(...)` — surface a SQL error to the caller

`sqlite3` is `modernc.org/sqlite/lib`, the generated C API bindings. It has no `SQLiteContext` type; `RegisterScalarFunction` and `FunctionContext` both come from `modernc.org/sqlite`.

`http_get` is registered with `nArg = 1`; `http_post` with `nArg = -1` (variadic, validated internally for 2..3 args). `RegisterScalarFunction` marks both non-deterministic (which is what these side-effecting functions need). Registration is global and one-shot: a second call with the same name returns `a function named %q is already registered`, so tests guard with `sync.Once`.

### Transport behavior

- **Shared client.** All UDF calls reuse one package-level `http.Client` with `Timeout: 5s` and a keep-alive `Transport` (`MaxIdleConns: 64`, `MaxIdleConnsPerHost: 16`). Per-call clients would leak Transports and skip connection reuse.
- **5-second timeout.** The UDF callback runs synchronously on the SQLite connection's goroutine. Inside a trigger fired from `commitBatch`, a hung endpoint would stall every write on that DB until timeout. The 5s cap bounds the stall.
- **1 MiB body cap.** `io.LimitReader(resp.Body, 1<<20)`. Without it, a slowloris target could push an arbitrarily large string into a row, which then becomes a bound value and may enter the cache.
- **Transport failures → NULL, not error.** A `http_post` in a trigger that can't reach its endpoint must not roll back the surrounding write. Type errors and wrong arity *do* return a SQL error — those are query bugs, not runtime conditions.
- **No URL allowlist.** Any bound parameter that flows into `http_get`/`http_post` is SSRF-capable. `SELECT http_get(?)` with a user-controlled bind is an open proxy. Treat these as privileged primitives.

### Caching interaction

`Classify` in `pkg/database/classify.go` scans the stripped SQL for `HTTP_POST` (case-insensitive) and forces `Cacheable: false` when found. This applies to:

- `SELECT http_post(...)` → `ReturnsRows: true, Cacheable: false`
- `WITH … SELECT http_post(...)` (no write keyword) → `ReturnsRows: true, Cacheable: false`

Without this, the second identical `sqlite_response` call within `cache_ttl` would return the cached first response and **silently skip the POST**. Triggers are unaffected (writes are never cached).

`http_get` is *not* de-cached — GET is nominally idempotent. If you point `http_get` at an endpoint with side effects, add `HTTP_GET` to `hasSideEffectingUDF` or use `cache 0` on the handler.

## Background Queries (`sqlite_bg`)

Dispatches SQL to a background goroutine, returns `202 Accepted` immediately with a job ID. Optionally POSTs/GETs to a webhook on completion.

```caddyfile
sqlite_bg {
    use     main
    query   "UPDATE users SET last_login = ? WHERE id = ?"
    bind    1 {http.request.time}
    bind    2 {http.vars.user_id}
    on_complete {
        url     http://example.com/callback
        method  POST
    }
}
```

**Module ID**: `http.handlers.sqlite_bg`. Registered via `sqlite.go:init` → `caddy.RegisterModule(plugin.SQLiteBG{})` and `httpcaddyfile.RegisterHandlerDirective("sqlite_bg", plugin.ParseBG)`. Ordered `before respond`.

### Lifecycle

The handler owns a **module-scoped `rootCtx`** (background-derived, cancelled only on `Cleanup`). The HTTP request context is **not** used for the work — `r.Context()` is cancelled as soon as `ServeHTTP` returns, which would kill every goroutine immediately after the 202.

- `ServeHTTP` acquires `m.mu`, checks `m.closed`, calls `m.wg.Add(1)`, snapshots `rootCtx`, and unlocks before spawning.
- `Cleanup` sets `closed = true` (rejecting new jobs with `503`), cancels `rootCtx`, waits up to `bgShutdownGrace` (10s) for `wg`, then releases the DB.
- `wg.Add` under the same mutex that guards `closed` is what makes `wg.Wait` in `Cleanup` correct — no `Add` can race with `Wait` once `closed` is set.

### Result store

Completed results are stored in a package-level `results` map keyed by job ID, with a `bgResultTTL` (5 min) `expiresAt`. A single sweeper goroutine (started once, `sweeperOnce`) evicts expired entries every minute. The map is bounded by the job rate × TTL, not by process uptime.

Results are **only stored when `on_complete.url` is set** — a bare `sqlite_bg` with no webhook has nothing to store and skips the map entirely.

### Webhooks

- 30-second timeout (`bgWebhookTimeout`) via `http.NewRequestWithContext`.
- POST body: `{"status":"ok"|"error","job_id":"..."}` with `Content-Type: application/json`.
- GET URL: `<url>?job_id=<id>&status=<ok|error>`.
- Failures log a warning; they do not affect the job result or the handler.
- Webhook deliveries increment `wg` so `Cleanup` waits for in-flight deliveries up to the grace period.

## Architecture & Data Flow

1. **Init** (`sqlite.go:init`) → `database.RegisterHTTPFuncs()` (UDFs, must precede any `OpenDB`), then `caddy.RegisterModule` for `SQLiteDBs`, `SQLiteResponse`, `SQLiteTemplateFuncs`, `SQLiteBG`, then `httpcaddyfile.RegisterGlobalOption("sqlite_db", …)`, `RegisterHandlerDirective("sqlite_response", …)`, `RegisterHandlerDirective("sqlite_bg", …)`, then `RegisterDirectiveOrder` for both handlers ("before respond").
2. **Caddyfile parsing** → `ParseSQLiteDBOption` stores per-DB config in the global `pending` map (keyed by alias) and returns an empty `httpcaddyfile.App` so Caddy injects the `sqlite` app into `AppsRaw`.
3. **Provision** → `SQLiteDBs.Provision` drains `pending`, validates no duplicate paths, then per DB: opens `*sql.DB`, applies migrations, starts the batcher (writable DBs only), starts `optimize` ticker (if configured), starts `NewBackupScheduler` (if configured).
4. **Request handling** → reads go through `DB.QueryCached` (shared LRU + single-flight); writes through `DB.WriteStmt` (batcher). `sqlite_bg` spawns from `rootCtx`; `sqlite_response` uses `r.Context()`.
5. **Cache invalidation** by generation counter (`gen atomic.Uint64`). Every successful write commit bumps `gen`; cache keys include `gen`, so old entries become unreachable atomically.

## Modules & Directives

| Caddy Module ID | Type | Registered by |
|---|---|---|
| `sqlite` | App | `caddy.RegisterModule(plugin.SQLiteDBs{})` |
| `http.handlers.sqlite_response` | Handler | `caddy.RegisterModule(plugin.SQLiteResponse{})` |
| `http.handlers.templates.functions.sqlite` | Template funcs | `caddy.RegisterModule(plugin.SQLiteTemplateFuncs{})` |
| `http.handlers.sqlite_bg` | Handler | `caddy.RegisterModule(plugin.SQLiteBG{})` |

| Caddyfile Directive | Kind | Registered by |
|---|---|---|
| `sqlite_db` | Global option | `httpcaddyfile.RegisterGlobalOption("sqlite_db", plugin.ParseSQLiteDBOption)` |
| `sqlite_response` | Handler | `httpcaddyfile.RegisterHandlerDirective("sqlite_response", plugin.ParseResponse)` |
| `sqlite_bg` | Handler | `httpcaddyfile.RegisterHandlerDirective("sqlite_bg", plugin.ParseBG)` |

Both handler directives are ordered `before respond`.

## SQL Classification (`pkg/database/classify.go`)

Runs once per `sqlite_response` / `sqlite_bg` at **Provision**, and per template call at request time. Comments (`--` line, `/* */` block) are stripped before classification.

| Leading keyword | IsWrite | Cacheable | ReturnsRows |
|---|---|---|---|
| `SELECT`, `EXPLAIN`, `VALUES` | ✗ | ✓ (✗ if `HTTP_POST` present) | ✓ |
| `PRAGMA` (no `=`, not a side-effect pragma) | ✗ | ✓ | ✓ |
| `PRAGMA` with `=` or in side-effect set | ✓ | ✗ | ✗ |
| `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, `UPSERT` | ✓ | ✗ | only if `RETURNING` |
| `CREATE`, `DROP`, `ALTER`, `VACUUM`, `ANALYZE`, `REINDEX`, `ATTACH`, `DETACH` | ✓ | ✗ | ✗ |
| `WITH` (no write keyword) | ✗ | ✓ (✗ if `HTTP_POST` present) | ✓ |
| `WITH` (write keyword present) | ✓ | ✗ | only if `RETURNING` |
| anything else | error | — | — |

**Side-effect pragmas** (classified as write even without `=`): `wal_checkpoint`, `optimize`, `incremental_vacuum`, `shrink_memory`, `cache_spill`, `analysis_limit`, `soft_heap_limit`. These mutate server state, so caching or pool routing would be wrong.

**Driver limitation**: `modernc.org/sqlite` (SQLite 3.45.3) rejects data-modifying CTEs — `WITH x AS (INSERT/UPDATE/DELETE … RETURNING …) SELECT …` fails at prepare with `near "INSERT": syntax error`. Plain `WITH … SELECT` and a top-level `WITH … INSERT/UPDATE/DELETE` both work, and `Classify` still routes a `WITH` that mentions a write keyword as a write.

## Output Formats

| Format | Content-Type (success and error) | Error body |
|---|---|---|
| `json` | `application/json` | `{"status":"error","message":"..."}` |
| `xml` | `application/xml` | `<error><message>...</message></error>` |
| `csv` | `text/csv` | blank |
| `plain` | `text/plain; charset=utf-8` | error text |
| `dynamic` | from `content_type` column | `content` if `http_code` present, else empty |

`dynamic` requires exactly one row with columns `content`, `content_type`, and optionally `http_code`. Validation is runtime — SQLite doesn't expose result column names on a prepared statement without executing it. Smoke-test before deploy.

## Critical Gotchas

### Security
- **No SQL interpolation** — only bound parameters (`?`, `:name`, `@name`, `$name`).
- Unresolved Caddy placeholders → `400` naming the bind.
- **`http_get` / `http_post` UDFs are SSRF-capable.** A bound parameter that reaches a URL is an open proxy. This is a privileged primitive; do not expose to untrusted input without an allowlist at the query layer.

### Binds
- Positional binds are **reordered by numeric name**; must be `1..N` with no gaps.
- Named binds deduplicate: one `bind` line per unique name, even if the SQL references it multiple times.
- First bind's name shape determines positional vs named mode for the whole block; mixing is a parse error.

### Write batching
- Batcher collects up to `batch_size` requests or `batch_timeout` duration, then commits in one `BEGIN…COMMIT`.
- One goroutine per DB. Read-only DBs have no batcher — `WriteStmt` falls through to `ExecContext`/`QueryContext` on the pool (and bumps `gen`).
- Every successful commit bumps `gen`. Failed commit → every reply gets an error; `gen` is **not** bumped.
- A failing *statement* (constraint violation, bad SQL) only fails its own reply — the batch still commits and the sibling writes land.
- Exception: a statement that resolves with `ROLLBACK` (an explicit `OR ROLLBACK`, or `ON CONFLICT ROLLBACK`) tears the transaction down under the driver. The batch's `COMMIT` then fails, so every reply errors and `gen` stays put, but sibling statements may still have been persisted — treat such statements as incompatible with batching.

### Cache
- LRU per DB, keyed by `FNV64a(gen, sqlText, args)`.
- Results >100 rows not cached.
- Single-flight: N concurrent misses → 1 query + N−1 waiters.
- `cache 0` bypasses the cache for that handler only; it doesn't evict other callers' entries.
- Template reads share the same cache; template writes go through `WriteStmt` and bump `gen`.
- External writers (`sqlite3` CLI, cron) don't bump `gen` — staleness bounded by `cache_ttl`.

### DSN & connection pool
- Pragma defaults (set in `pkg/database/store.go:buildDSN`): `journal_mode(WAL)`, `synchronous(NORMAL)`, `busy_timeout(5000)`, `foreign_keys(ON)`, `cache_size(-32000)`, `mmap_size(268435456)` (skipped for `:memory:`), `temp_store(MEMORY)`, `wal_autocheckpoint(2000)`. Read-only opens set `mode=ro` and skip WAL/sync/mmap.
- Pool: `SetMaxOpenConns = SetMaxIdleConns = min(NumCPU, 8) * 2 + 1`, `SetConnMaxLifetime(0)`.
- `:memory:` is rewritten to `file::memory:?cache=shared` so every pooled connection sees the same DB. The database lives only while at least one connection is open, so the pool keeps idle connections (`SetMaxIdleConns` = `SetMaxOpenConns`); closing the last one discards the data.
- `{env.X}` in paths is expanded at provision; a missing variable is a hard error (`expandEnv` returns an error, which `Provision` surfaces), so the DB never opens at a literal `{env.X}` path.

### `read_only` DBs
- Cannot be migrated, backed up, or optimized: `migrate`, `optimize`, and `backup` each error at provision when combined with `read_only`.

### Provision vs request-time
- Classification, bind validation, DB lookup, prepared-statement smoke test happen at **Provision**.
- Request path is: replacer → execute → format. No re-classification.
- Errors at Provision prevent Caddy from starting — you find out before going live.

### Go semantics
- `string(int)` converts to a Unicode rune, not decimal digits. Use `fmt.Sprintf("%d", n)`.
- `atomic.Uint64` uses `.Add(n)`, not `.Inc()`.
- Register UDFs via `sqlite.RegisterScalarFunction(name, nArg, callback)` — globally once, before any `OpenDB`.
- UDF callback: `func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error)` from `modernc.org/sqlite`. Return `(nil, nil)` for NULL and `(nil, err)` for a SQL-level failure; there is no `ResultText`/`ResultNull`.

## Known Bugs / Open Items

None open. The items previously listed here are all fixed, and each one has a guard:

| Was | Now |
|---|---|
| `pragmaName` case sensitivity | keyword stripped case-insensitively; `TestClassifyPragmaLowercaseSideEffect` |
| Cache key arg collision | args written as `%T:%v\x00`; `TestKeyArgsBoundary` |
| `{env.X}` missing var | `expandEnv` returns an error; `Provision` fails |
| `read_only` + `backup`/`optimize` | both error at provision, like `migrate` |
| `backup.method` ignored | non-`vacuum_into` rejected at parse time |
| `Lookup` on unknown alias | returns an error naming the alias |
| `dynamic` scan order | columns mapped by name; unknown/duplicate columns error |
