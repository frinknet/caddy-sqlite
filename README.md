# caddy-sqlite

Imagine if Caddy was more than just a web server in front of your app. If all you need is basic CRUD for a single tenant without world-domination scale, SQLite may be the perfect database. On their own, neither is a backend. Together Caddy becomes your application. No proxies. No ORM. No separate container. No orchestrate. **One binary, one config, one database... DONE!**

This module gives you the SQL primitive that makes it possible: batched writes, a shared per-database cache, five output formats, versioned migrations, scheduled backups, and a `{{ sqlite }}` template function that shares the cache with every handler. Write the query, bind the params, ship.

The rest of the stack you already have. `templates` and `file_server` are Caddy built-ins. If you pair it with `upload` and `exec` you can get some really slick applications without a DevOps team or headaches.

## Scope

**In scope:**
- Parameterized reads and writes against one or more SQLite databases
- Named or positional binds, no SQL string interpolation
- Group-commit write batching with per-DB tuning
- Shared per-DB result cache invalidated by generation counter
- Output formats: JSON, XML, CSV, plain, dynamic
- Versioned schema migrations from a directory of `.sql` files
- Template function sharing the query cache with handlers
- Scheduled `PRAGMA optimize`
- Scheduled backups via `VACUUM INTO`

**Out of scope:**
- File storage (use `upload` + `file_server`)
- Async process spawning on DB writes (use `exec` in a handler chain)
- Subdomain routing (use `reverse_proxy` + matchers)
- Cert storage (use `file_system`)
- Body parsing (put `jsonparse`, `request_body`, or `reverse_proxy` upstream)
- User-defined SQL functions
- Cross-database queries (no `ATTACH`)

The scope is deliberate. This is the SQL primitive. Everything else Caddy already does better.

## Naming

Top-level directives are prefixed `sqlite_`. Sub-directives inside blocks are bare.

Inside `sqlite_db`: `batch_size`, `batch_timeout`, `cache_ttl`, `cache_max`, `migrate`, `read_only`, `backup`, `optimize`.

Inside `sqlite_response`: `use`, `query`, `bind`, `cache`, `output`.

## In-memory databases

`sqlite_db scratch file::memory:?cache=shared` opens a shared volatile workspace. All connections in the pool attach to the same in-memory database, and every `sqlite_response` and template call on `scratch` sees the same data.

**This is the right place for cross-request scratch** — rate-limit counters, ephemeral job queues, session state, memoized values — that you do not want to persist. No WAL, no fsync, no disk I/O, no backup to worry about.

- Data is lost on process exit.
- Migrations run on every start, so the schema is reproducible from source.
- Writes bump the generation counter, invalidating any cached reads on the same DB.
- Plain `:memory:` (without `cache=shared`) creates a separate private DB per connection. The module rewrites it to the shared URI at open, so you get the behavior you meant.

## Directives

### `sqlite_db` — global block

Declares a database. Multiple allowed. Each declaration opens one `*sql.DB`, shared by every handler and template call that references it. **One pool, one batcher, one cache, one generation counter per database** — regardless of how many endpoints touch it.

```caddyfile
{
    sqlite_db <name> <path> {
        batch_size    <int>       # 1..4096, default 128
        batch_timeout <duration>  # >0 and <=1s, default 5ms
        cache_ttl     <duration>  # default 5s; 0 disables
        cache_max     <int>       # default 1000
        migrate <dir>             # directory of NNNN_*.sql files
        read_only                 # opens mode=ro; skips probe, batcher, migrate, backup
        backup { ... }
        optimize <duration>       # e.g. 24h
    }
}
```

| Option | Type | Default | Notes |
|---|---|---|---|
| `batch_size` | int | `128` | Writes per transaction. |
| `batch_timeout` | duration | `5ms` | Batcher wait before commit. |
| `cache_ttl` | duration | `5s` | Shared query cache TTL. `0` disables. |
| `cache_max` | int | `1000` | Max entries in the DB's cache. |
| `migrate <dir>` | — | disabled | Required arg. Reads `.sql` files from `<dir>` at startup. |
| `read_only` | flag | false | `mode=ro`; skips probe, batcher, migrate, backup. |
| `backup` | block | — | Scheduled online backup. |
| `optimize <dur>` | duration | disabled | Runs `PRAGMA optimize` on the writer at the given interval. |

**Path resolution:** `<path>` is literal. `{env.X}` placeholders resolve from the process environment at provision; missing variables are a hard error at `caddy validate`.

**Aliases:** `<name>` is the identifier handlers reference via `use`. Dangling references error at provision. Two aliases pointing at the same file is also an error — no accidental double-pooling.

**`read_only` is a hard guarantee.** Any `sqlite_response` writing to a read-only DB fails at `Provision`, not at request time. You find out before you go live.

### `sqlite_response` — handler block

Executes one prepared query per request. **Reads run on the pool; writes go through the batcher; reads optionally go through the shared cache.**

```caddyfile
sqlite_response {
    use    <name>            # required
    query  "<SQL>"           # required; exactly one statement
    bind   <name> <value>    # zero or more
    cache  <0|1>             # 0 = bypass cache for this handler; default 1
    output json|xml|csv|plain|dynamic  # default json
}
```

| Option | Notes |
|---|---|
| `use <name>` | Required. Which declared DB. Each handler runs against exactly one DB. |
| `query "<SQL>"` | Exactly one statement. |
| `bind <name> <value>` | See Binds. Order defines positional binding order. |
| `cache 0` | Bypass the shared cache for this handler. Reads still run; they just don't populate or consult the LRU. |
| `output <fmt>` | `json`, `xml`, `csv`, `plain`, `dynamic`. |

**Reads vs writes.** Classification runs once at `Provision` on the static SQL string. Bind values are never scanned, substituted, or inspected — the query you wrote is the query that runs, and the classification happens before any request arrives.

- `SELECT`, `EXPLAIN`, `VALUES` → read; cached.
- `PRAGMA name` (no `=`, not in the side-effect list) → read; cached.
- `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, `UPSERT`, `CREATE`, `DROP`, `ALTER`, `VACUUM`, `ANALYZE`, `REINDEX`, `ATTACH`, `DETACH` → write; through the batcher, not cached.
- `INSERT/UPDATE/DELETE ... RETURNING` → write; through the batcher with `QueryContext`, rows returned, not cached.
- `PRAGMA name = value` → write; through the batcher, not cached.
- `PRAGMA wal_checkpoint`, `optimize`, `incremental_vacuum`, `shrink_memory`, `cache_spill`, `analysis_limit`, `soft_heap_limit` → write; through the batcher, not cached. **These have side effects even without `=`** — a classifier that only looked for `=` would misroute `PRAGMA wal_checkpoint` as a read.
- `WITH` (no write keyword after stripping comments) → read; cached.
- `WITH` (write keyword present) → uncached. If `RETURNING` is present, routes through the batcher (`gen` bumps on commit); otherwise runs on the pool.

Leading and inline `--` and `/* */` comments are stripped before classification.

## Binds

**Values are never interpolated into SQL. They are supplied as bound parameters.** This is the module's only non-negotiable security property, and it's why there is no `{http.vars.x}` substitution in query text the way you might expect from a Caddy module.

### Positional

Names are `1`, `2`, `3`, ... in order. SQL uses `?`.

```caddyfile
sqlite_response {
    use   main
    query "SELECT id, email FROM users WHERE tenant = ? AND status = ?"
    bind  1 {http.request.header.X-Tenant}
    bind  2 active
}
```

The count of `?` must match the number of `bind` lines; `Provision` enforces this. **Positional binds are reordered by their number**, so `bind 2 {b}` and `bind 1 {a}` bind correctly as `[a, b]`.

### Named

Names are identifiers. SQL uses `:name`, `@name`, or `$name`.

```caddyfile
sqlite_response {
    use   main
    query "SELECT * FROM events WHERE tenant = :tenant AND (created_by = :user OR assigned_to = :user)"
    bind  tenant {http.request.header.X-Tenant}
    bind  user   {http.vars.user_id}
}
```

**Named parameters may repeat in SQL; bind once.** That's the whole point of named binds — a query that mentions the same value three times still only requires one `bind` line.

### Mixing

Rejected. The first bind's name shape determines the mode; a bind of the wrong shape is a parse error.

### Values

Each bind value is a Caddy placeholder template, resolved with `repl.ReplaceAll(tmpl, "")` at request time. Unresolved placeholders return `400` naming the offending bind — that's an operator error (a `vars` line that never ran), so the client gets told.

**All resolved values are strings.** SQLite applies type affinity to string-bound parameters. Cast in SQL when type matters: `CAST(? AS INTEGER)`, `strftime(?, ...)`. This is not a limitation — it's what "no SQL interpolation" means. The value you bind is the value SQLite sees.

**Escaping:** none required. Binds go to the driver as `database/sql` parameters; the replacer substitution result is bound verbatim.

## Output formats

Set with `output`. Default `json`.

### `json`

```json
{"status": "ok", "data": {"rows": [...]}}
{"status": "ok", "data": {"affected": 1}}
{"status": "error", "message": "unresolved bind 2: {json.email}"}
```

### `xml`

```xml
<response status="ok"><rows><row><id>1</id></row></rows></response>
<error><message>unresolved bind 2: {json.email}</message></error>
```

### `csv`

Header row from column names, one row per record, `nil` → empty field. Error: blank body, status set.

### `plain`

One field per line, blank line between rows. `nil` → empty line. Error: error text as body, status set.

### `dynamic`

**Serves complete responses from the database** — status code, content type, and body — via a query that returns exactly one row with 2 or 3 columns:

- `content` (BLOB or TEXT) — required
- `content_type` (TEXT) — required
- `http_code` (INTEGER) — optional

```caddyfile
sqlite_response {
    use    media
    query  "SELECT body, content_type, http_code FROM responses WHERE path = ?"
    bind   1 {http.request.uri.path}
    output dynamic
    cache  0
}
```

- `content` is the response body.
- `content_type` becomes the `Content-Type` header verbatim.
- `http_code` (when present) becomes the status code — for success *and* error paths. A table of pre-built pages, error states, or file blobs each carry their own response metadata.

Validation is runtime — SQLite doesn't expose result column names on a prepared statement without executing it. Smoke-test the endpoint once before deploy.

**The module does not default to `application/octet-stream`** when `content_type` is empty. An empty type is a data problem, not a header the client should trust.

### Content-Type per format

| Output | Success | Error | Error body |
|---|---|---|---|
| `json` | `application/json` | `application/json` | `{"status":"error","message":"..."}` |
| `xml` | `application/xml` | `application/xml` | `<error><message>...</message></error>` |
| `csv` | `text/csv` | `text/csv` | blank |
| `plain` | `text/plain; charset=utf-8` | `text/plain; charset=utf-8` | error text |
| `dynamic` | from `content_type` | from `content_type` | `content` if `http_code` present |

## Caching

**Reads only. Writes bypass the cache — they are the invalidation.**

**One cache per database.** Keyed on `(generation, sql, args)`. `sqlite_response` and the `sqlite` template function share it: the same query with the same arguments populated by one endpoint is served from cache by another, and by templates.

**Enabled by default** at `5s` TTL / 1000 entries per DB. Configure with `cache_ttl` and `cache_max` in `sqlite_db`. Disable per-handler with `cache 0` in `sqlite_response` — that endpoint always queries directly, regardless of what other callers have cached.

**Invalidation is by generation counter, not key inspection.** Every successful write commit increments the DB's `gen`. Cache keys include `gen`, so a write makes every old entry unreachable — no scan, no clear, no race between clear and store. Template writes bump `gen` too, through the same `WriteStmt` path as handlers.

**Thundering herd is single-flight'd per key.** When N concurrent callers miss the same key, one runs the query, N−1 wait. This matters at scale; without it, a cache miss under load is N queries, not one.

**Large results aren't cached.** Result sets over 100 rows skip the cache; listing endpoints don't repeat enough to justify memory.

**External writers.** `gen` only tracks writes through the module. Writes from `sqlite3` CLI or a cron job won't invalidate until TTL. Set `cache_ttl` to your maximum tolerable staleness for external writes.

## Migrations

Opt-in via `migrate <dir>` in `sqlite_db`. Uses `PRAGMA user_version` as the applied-version cursor. **Each migration runs in a transaction with the version bump**; a crash mid-migration is impossible to corrupt.

**File naming:** `NNNN_description.sql` (`0001_init.sql`, `0002_add_assets.sql`). The numeric prefix is parsed as an integer for ordering. Non-matching files are ignored (put `README.md` alongside safely). Gaps allowed.

**On-disk only.** `migrate` requires a path; there is no embedded mode. `.sql` files are read from the given directory at provision. If you want your migrations baked into the binary, put them on a read-only mount alongside your Caddyfile — Docker images handle this naturally.

**Semantics:**

- Fresh DB (`user_version = 0`): all migrations apply in ascending order.
- Existing DB (`user_version = N`): only migrations with version > N apply.
- `:memory:` / `file::memory:?cache=shared`: fresh on every start; all migrations apply every time.
- Failure: the migration rolls back; version unchanged; `Provision` errors; Caddy fails to start.

**Never edit a shipped migration.** Once applied anywhere, its content is frozen. The runner doesn't checksum, so an edit silently does nothing on already-migrated DBs and something different on fresh DBs. This rule is what keeps schema deterministic across environments.

**Forward-only.** No down migrations. Backups are the recovery path — which is why `backup` exists.

**Example:**

```
migrations/
├── 0001_init.sql
├── 0002_add_assets.sql
└── README.md
```

```sql
-- 0002_add_assets.sql
CREATE TABLE assets (
    id       INTEGER PRIMARY KEY,
    tenant   TEXT NOT NULL,
    rel_path TEXT NOT NULL UNIQUE
);
CREATE INDEX assets_tenant ON assets(tenant, id DESC);
```

Multi-statement files work. **Do not** add `BEGIN`/`COMMIT` inside the file; the runner wraps each file in its own transaction, and nesting would break the atomicity guarantee.

## Backups

Opt-in via `backup` in `sqlite_db`. Uses `VACUUM INTO`, which reads the source in a single read transaction, handles WAL correctly, and writes a defragmented file **without blocking the writer**. This is the right primitive for online backups of a live SQLite database — `cp` of a live file is not.

```caddyfile
sqlite_db media /srv/media.db {
    backup {
        schedule 0 3 * * *       # cron: 03:00 daily
        dir      /srv/backups
        retain   7               # keep 7 most recent; 0 = unlimited
    }
}
```

| Option | Notes |
|---|---|
| `schedule` | Cron expression. Five fields: minute, hour, day-of-month, month, day-of-week. |
| `dir` | Directory for backup files. Created if missing. |
| `retain` | Number of most-recent backups to keep. `0` keeps all. |

Each run writes `<name>-<timestamp>.db` in `dir`. **Does not overwrite an existing file** — unique name per run. Requires SQLite 3.27.0+.

**Schedule:** one goroutine per DB with a cron scheduler. Backups run on a dedicated connection, not through the batcher. A failed backup logs a warning and retries on the next tick; it does not affect the request path.

**Retention:** `retain N` keeps the N most recent files matching `<name>-*.db` in `dir`, deleting older ones after a successful backup.

**Read-only DBs** skip the backup block — there is nothing to back up that isn't already static.

**What this does not do:** it does not restore. Restoring is a file copy while Caddy is stopped. Backups are for recovery, not hot-standby.

## Template function

Registered as `http.handlers.templates.functions.sqlite` for the `templates` middleware. Positional arguments only.

```caddyfile
templates {
    extensions {
        sqlite
    }
}
```

```html
{{ range sqlite "main" "SELECT name, email FROM users WHERE tenant = ? AND status = ?" .Tenant "active" }}
    <li>{{ .name }} — {{ .email }}</li>
{{ end }}
```

**Behavior:**

- Arg 1: DB name (as declared in `sqlite_db`).
- Arg 2: SQL, `?` positional binds only.
- Remaining args: bound positionally.
- Returns `[]map[string]any` — same shape as `sqlite_response` rows.

**Reads share the per-DB cache.** The same query with the same args, whether from a handler or a template, hits the same LRU. A dashboard template running the same user query as an API endpoint is one database query per cache window, not two.

**Writes go through the batcher.** The SQL is classified the same way as `sqlite_response`; writes route through `WriteStmt`, commit a batch, and bump `gen`. `RETURNING` is supported. Non-RETURNING writes return an empty slice, so templates don't have to special-case the result.

**Types:** template arguments bind verbatim — `int64` as `int64`, `string` as `string`. Unlike `sqlite_response` (where every value is a string from the replacer), the template function preserves types. That's the right behavior for a Go-side API — you have real typed values, pass them.

**Context:** the function uses `context.Background()`. Queries block until they finish; single-flight waiters are not cancelable by client disconnect. Acceptable for template rendering.

## Optimize

Opt-in via `optimize` in `sqlite_db`. Runs `PRAGMA optimize` on the writer connection at the configured interval.

```caddyfile
sqlite_db main /srv/app.db {
    optimize 24h
}
```

`PRAGMA optimize` is a near-no-op that occasionally runs `ANALYZE` on a table whose statistics are stale. It writes `sqlite_stat1` — **it is a write, not a read**, which is why the classifier treats it as one.

The module runs it on the writer connection, once per interval, in a background goroutine. It doesn't go through the batcher (single statement, once a day) and doesn't bump `gen` (there are no cacheable results to invalidate — `sqlite_stat1` affects query planning, not query results).

**Do not put `PRAGMA optimize` in a `sqlite_response`** — at request rate it's a no-op with per-request overhead. This sub-directive exists precisely to prevent that anti-pattern.

## Tuning reference

Defaults target a single-tenant deployment with fast queries and low write rates, **with headroom toward 10k concurrent connections**. The numbers below are the ones worth changing first.

### DSN pragmas (set by the module; not configurable)

| Pragma | Value | Why |
|---|---|---|
| `busy_timeout` | `5000` | 5s retry window on lock contention. |
| `journal_mode` | `WAL` | Concurrent reads + one writer. Mandatory. |
| `synchronous` | `NORMAL` | ~2× write throughput vs `FULL`; survives process crashes. |
| `foreign_keys` | `ON` | Cheap integrity. |
| `cache_size` | `-32000` | 32 MB per connection. |
| `mmap_size` | `268435456` | 256 MB memory-mapped I/O. |
| `temp_store` | `MEMORY` | Sort/group temp tables stay in RAM. |
| `wal_autocheckpoint` | `2000` | ~8 MB WAL between checkpoints. |

These defaults are chosen for the 90th-percentile web workload: mostly reads, bursts of writes, low contention. They are the difference between "SQLite is fine" and "SQLite is doing the same amount of work as Postgres with none of the operational cost."

### Connection pool (set by the module)

`SetMaxOpenConns(N*2+1)`, `SetMaxIdleConns(N*2+1)`, `SetConnMaxLifetime(0)` where `N = min(NumCPU, 8)`. One writer (the batcher) plus readers with 2× headroom. `Idle == Open` avoids cold-open latency.

### Per-DB knobs

| Workload | `batch_size` | `batch_timeout` | `cache_ttl` |
|---|---|---|---|
| Low traffic (<100 writes/s) | 32 | 1ms | 10s |
| Default web app | 128 | 5ms | 5s |
| High-write (event logs, imports) | 512 | 10ms | 1s |
| Read-heavy, rarely written | 128 | 5ms | 30s |
| Bulk import only | 4096 | 100ms | 0 |

### Handler knobs

| Endpoint | `cache` |
|---|---|
| Listing, rarely changes | default (`1`) |
| Detail, user-specific | default (`1`) |
| Near-real-time | `0` (bypass) |
| Write-only | `0` (no reads to cache) |

## Full example

```caddyfile
{
    sqlite_db main /srv/app.db {
        batch_size    128
        batch_timeout 5ms
        cache_ttl     5s
        cache_max     1000
        migrate       /etc/caddy/migrations
        optimize      24h
        backup {
            schedule 0 3 * * *
            dir      /srv/backups
            retain   7
        }
    }

    sqlite_db media /srv/media.db {
        batch_size    256
        batch_timeout 2ms
        cache_ttl     10s
        cache_max     500
        migrate       /etc/caddy/media-migrations
        backup {
            schedule 0 */6 * * *
            dir      /srv/backups/media
            retain   14
        }
    }

    sqlite_db scratch file::memory:?cache=shared {
        migrate /etc/caddy/migrations
    }
}

example.com {
    handle /api/users {
        sqlite_response {
            use   main
            query "SELECT id, email, status FROM users WHERE tenant = :tenant AND status = :status"
            bind  tenant {http.request.header.X-Tenant}
            bind  status active
        }
    }

    handle /api/signup {
        sqlite_response {
            use   main
            query "INSERT INTO subscribers (tenant, email) VALUES (?, ?) ON CONFLICT(tenant, email) DO NOTHING"
            bind  1 {http.request.header.X-Tenant}
            bind  2 {json.email}
        }
    }

    handle /api/assets {
        sqlite_response {
            use   media
            query "INSERT INTO assets (tenant, rel_path) VALUES (:tenant, :path) RETURNING id"
            bind  tenant {http.request.header.X-Tenant}
            bind  path   {http.request.uri.path}
        }
    }

    handle /api/export {
        sqlite_response {
            use    main
            query  "SELECT id, email, created_at FROM subscribers WHERE tenant = ?"
            bind   1 {http.request.header.X-Tenant}
            output csv
            cache  0
        }
    }

    handle /blob/* {
        sqlite_response {
            use    media
            query  "SELECT content, content_type, http_code FROM files WHERE path = ?"
            bind   1 {http.request.uri.path}
            output dynamic
            cache  0
        }
    }

    handle /dashboard/* {
        templates {
            extensions {
                sqlite
            }
        }
        file_server {
            root /srv/templates
        }
    }
}
```

## Implementation notes

**Package layout:**

```
pkg/
├── database/   # DSN, pool, batcher, classifier, migrations, backups
├── cache/      # LRU + typed key
└── plugin/     # Caddy integration: sqlite_db, sqlite_response, template function

sqlite.go       # module registration
```

**`pkg/database` and `pkg/cache` have zero Caddy imports.** `pkg/plugin` is the only package that imports `caddyserver/caddy`. The engine is testable without a Caddy context and reusable in a native server if you ever want one.

**Shared pool:** each resolved path has one `*DB{db, batcher, cache, flight, gen, refs}` in a process-global registry. Handlers acquire a ref in `Provision`, release in `Cleanup`. The last release closes the batcher (draining pending writes) then the DB.

**Generation counter:** `atomic.Uint64` per `*DB`. The batcher's commit path bumps it after `tx.Commit()` succeeds. Template writes bump it the same way (through `WriteStmt`). Cache keys include `gen`; `gen++` invalidates everything atomically — no scan, no clear, no race.

**Errors go to logs, not responses.** Driver errors name tables and columns; clients get the format-appropriate error body. The one exception is `400` — unresolved binds are the operator's fault and the client needs to know.

## Build

```bash
xcaddy build --with caddysqlite=.
caddy validate --config Caddyfile
```

## Known caveats

1. **`WITH` statements run but may not cache.** Classification strips comments and scans for `\b(INSERT|UPDATE|DELETE|REPLACE|UPSERT|CREATE|DROP|ALTER|VACUUM|ANALYZE|REINDEX|ATTACH|DETACH)\b`. If found, the statement runs with `QueryContext`, uncached. If `RETURNING` is present, it routes through the batcher; otherwise it runs on the pool. A read with a bare enum value like `'INSERT'` triggers the scan and skips caching; a read mentioning `CREATE_user` does not, because `_` is a word character.
2. **Binds resolve to strings.** Cast in SQL when type matters. `CAST(? AS INTEGER)` is idiomatic.
3. **Template reads share the DB cache; template writes go through the batcher.** Both paths classify the SQL, so `{{ sqlite ... }}` behaves like a `sqlite_response` call.
4. **`read_only` DBs can't be migrated, backed up, or optimized.** Configuring any of those on a `read_only` DB fails at provision.
5. **`:memory:` uses `file::memory:?cache=shared`.** Plain `:memory:` creates a separate private DB per connection; the module rewrites it to the shared URI. Data is volatile; migrations run every start.
6. **Migrations are forward-only.** Backups are the recovery path.
7. **`use` is required on every `sqlite_response`.** There is no default DB; each handler runs against exactly one DB per request.
8. **No cross-DB queries.** The module opens one `*sql.DB` per alias and does not use SQLite's `ATTACH`. `main.users` in a query refers to SQLite's default schema in the current DB, not a different alias.
9. **CSV errors are blank-bodied; plain errors are the error text.** Clients must check the status code to distinguish success from failure. `dynamic` errors return the `http_code` value if present, else `500` with an empty body.
10. **`dynamic` validates at runtime, not provision.** Column names and row count are checked on the first request. Smoke-test the endpoint once before deploy.
11. **`backup` is for recovery, not hot-standby.** Restoring is a file copy while Caddy is stopped.
12. **`cache 0` on a handler does not clear entries populated by other callers.** It only bypasses the cache for requests through that handler. To invalidate a specific query across all callers, wait for TTL or write to the DB (any write bumps `gen` and invalidates everything).
13. **`WITH ... DML` without `RETURNING` doesn't bump `gen`.** It runs on the pool (autocommit), commits the write, and returns `{"rows": []}`. Cached reads on that DB go stale until TTL.
14. **`PRAGMA optimize` must not be a `sqlite_response` query.** Use the `optimize` sub-directive. At request rate, `PRAGMA optimize` is a no-op with per-request overhead; as a scheduled task, it's the SQLite-recommended maintenance pattern.
