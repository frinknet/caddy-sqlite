# **One binary, one config, one database... DONE!**

[![ci](https://github.com/frinknet/caddy-sqlite/actions/workflows/ci.yml/badge.svg)](https://github.com/frinknet/caddy-sqlite/actions/workflows/ci.yml)

A Caddy v2 plugin that exposes SQLite as a first-class backend: one statement per
route, batched writes, a shared query cache, versioned migrations, scheduled
backups, and cross-schema joins — all from the Caddyfile, with no SQL
interpolation and no application server.

## Scope

`caddy-sqlite` gives you the SQL primitive and gets out of the way:

- **One statement per request.** `sqlite` runs exactly one classified SQL
  statement and serialises the result. No ORM, no query builder, no migrations
  DSL — just the SQL you wrote, bound with parameters.
- **Writes are batched.** Concurrent writes to the same database are grouped
  into a single transaction, so write-heavy endpoints do not serialise into one
  commit per request.
- **Reads are cached.** One shared LRU per process, invalidated the moment a
  write commits.
- **Every declared database is attached as a schema**, so a single query can join
  across databases — and unqualified names land in an in-memory scratch schema.
- **Read-only databases are a hard guarantee**, migrations run once at boot, and
  backups use `VACUUM INTO`.

## Naming

`sqlite` is both the global declaration block and the per-route handler. Caddy
keeps global options and handler directives in separate registries, so the same
word is unambiguous by position: at the top of the Caddyfile it declares
databases, inside a site it runs one statement.

```caddyfile
{
	sqlite {
		db app /var/lib/caddy/app.db
	}
}

example.com {
	handle /api/notes {
		sqlite {
			query  "SELECT id, body FROM app.notes ORDER BY id DESC"
			output json
		}
	}
}
```

## In-memory databases

The scratch schema is the read pool's own `main`: a shared in-memory database
addressed as `main.*`. It is never written to disk and never migrated by itself —
you give it tables with a `db main { migrate <dir> }` block:

```caddyfile
sqlite {
	db main {
		migrate /srv/www/scratch-migrations
	}
}
```

Because it lives in memory, its contents disappear on reload. Use it for
cross-request scratch — counters, memoized lookups, an idempotency key — not for
anything you need after a restart.

## Directives

### `sqlite` — global block

Declares the databases and the engine-wide knobs.

```caddyfile
sqlite {
	batch_size    <int>       # 1..4096, default 128
	batch_timeout <duration>  # >0 and <=1s, default 5ms
	cache_ttl     <duration>  # default 5s; 0 disables caching
	cache_max     <int>       # default 1000
	timeout       <duration>  # default 0 (no deadline)

	db <name> <file> {
		migrate  <dir>            # directory of NNNN_*.sql files
		readonly                  # migrate once on open, then serve mode=ro
		optimize <duration>       # e.g. 24h
		backup { ... }
	}

	db main {
		migrate <dir>             # the in-memory scratch schema: no file
	}
}
```

| Option | Type | Default | Notes |
|---|---|---|---|
| `batch_size` | int | `128` | Writes per transaction, applied per database. |
| `batch_timeout` | duration | `5ms` | How long a batch waits before committing. |
| `cache_ttl` | duration | `5s` | TTL of the shared read cache. `0` disables it. |
| `cache_max` | int | `1000` | Entries in the shared cache. |
| `timeout` | duration | disabled | Deadline for every statement: reads, writes, `dynamic`, templates and background jobs. |
| `db <name> <file>` | — | required | Declares a database. `<name>` is the schema every query uses; `<file>` is the SQLite file. |
| `migrate <dir>` | — | disabled | Required arg. Reads `.sql` files from `<dir>` at startup. |
| `readonly` | flag | false | Migrate once on open, then attach `mode=ro`. |
| `optimize <dur>` | duration | disabled | Runs `PRAGMA optimize` on that database's writer. |
| `backup { ... }` | block | — | Scheduled online backup. |

**Database names** are SQLite schema names, so they must be identifiers, and
`main` and `temp` are reserved. Declaring is attaching: there is no separate
attach step, and every declared database is visible to every query.

**Schema names are matched the way SQLite matches them: case-insensitively.**
`INSERT INTO "APP".notes`, `App.notes` and `app.notes` all route to the declared
`app`, and `MAIN.hits` reaches the scratch schema. A name that matches no
declaration is a config error, as is a write to a `readonly` one.

**Path resolution:** `<file>` is literal. `{env.X}` placeholders resolve from the
process environment at provision; a missing variable is a hard error at
`caddy validate`.

**Aliases:** two `db` blocks pointing at the same file are an error — no
accidental double-pooling.

### `sqlite` — handler block

Executes one prepared statement per request.

```caddyfile
sqlite {
	query  "<SQL>"           # required; exactly one statement
	bind   <name> <value>    # zero or more
	output json|xml|csv|plain|dynamic|none   # default json
	status <code>            # optional; defaults to 200
	cache  <0|1>             # 0 = bypass the shared cache for this handler
	continue                 # fire-and-forget: dispatch, then the next handler answers
}
```

| Option | Notes |
|---|---|
| `query "<SQL>"` | Exactly one statement. |
| `bind <name> <value>` | See Binds. Order defines positional binding order. |
| `output <fmt>` | `json`, `xml`, `csv`, `plain`, `dynamic`, or `none`. |
| `status <code>` | Overrides the response status (100..599). |
| `cache 0` | Bypass the shared cache for this handler. Reads still run; they just do not populate or consult the LRU. |
| `continue` | Do not answer. Dispatch the statement in the background and call the next handler, which owns the response. Cannot be combined with `output` or `status`. |

**Schemas, not `use`.** There is no per-handler database selection. Every query
addresses its data by schema:

```caddyfile
sqlite {
	query  "SELECT n.id, n.body, m.name AS asset
	        FROM app.notes n LEFT JOIN media.assets m ON m.id = n.asset_id"
	output json
}
```

Unqualified table names resolve to the scratch schema, so a query that forgets its
schema gets SQLite's `no such table: notes` at request time.

**Reads vs writes.** Classification runs once at `Provision` on the static SQL
string. Bind values are never scanned, substituted, or inspected — the query you
wrote is the query that runs, and the classification happens before any request
arrives.

- `SELECT`, `EXPLAIN`, `VALUES` → read; cached.
- `PRAGMA name` (no `=`, not in the side-effect list) → read; cached.
- `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, `UPSERT` → write; through the
  database's batcher, not cached.
- `INSERT/UPDATE/DELETE ... RETURNING` → write; through the batcher with
  `QueryContext`, rows returned, not cached.
- `CREATE`, `DROP`, `ALTER`, `VACUUM`, `ANALYZE`, `REINDEX`, `ATTACH`, `DETACH` →
  write.
- `PRAGMA name = value` → write; counts as a scratch write.
- `PRAGMA wal_checkpoint`, `optimize`, `incremental_vacuum`, `shrink_memory`,
  `cache_spill`, `analysis_limit`, `soft_heap_limit` → write. **These have side
  effects even without `=`** — a classifier that only looked for `=` would
  misroute `PRAGMA wal_checkpoint` as a read.
- `WITH` (no write keyword after stripping comments) → read; cached.
- `WITH` (write keyword present) → write.

Leading and inline `--` and `/* */` comments are stripped before classification.

**Writes must name exactly one schema.** The target is resolved at `Provision`
from the statement itself — `INSERT/REPLACE INTO`, `UPDATE`, `DELETE FROM`,
`CREATE`/`DROP`/`ALTER`/`REINDEX`/`ANALYZE`, `VACUUM <schema>` — and enforced:

- no schema → config error (`writes must name a schema`); use `main.` for scratch;
- a `readonly` database → config error;
- two schemas in one statement → config error, because SQLite cannot make a
  transaction spanning attached databases atomic in WAL mode, and batching would
  otherwise make that routine rather than rare.

Reads are not parsed: they may touch as many schemas as they like, and a wrong
name is SQLite's error to report.

A ready-made CRUD resource built on this directive — list, create, fetch, update,
patch and delete with the right status codes — ships in `www/rest.caddyfile`;
see [Reusable REST pattern](#reusable-rest-pattern).

**`continue`** turns the handler into a producer for the rest of the chain:

```caddyfile
route /api/events {
	sqlite {
		query    "INSERT INTO app.events (kind) VALUES (:kind)"
		bind     kind {http.request.body}
		continue
	}
	respond "queued" 202
}
```

Dispatch failures — an unresolved bind, or a dispatch during shutdown — are
returned as errors so the chain (and `handle_errors`) sees them; the handler never
writes a body of its own, because the response belongs to whatever runs next.

## Binds

Each `bind <name> <value>` is a Caddy placeholder template. At request time every
`{...}` in the template is looked up in the replacer first: an unknown placeholder
returns an error naming the offending bind instead of silently binding an empty
string.

Because the check inspects the template rather than the result, a body that
happens to contain braces binds fine: `bind body {http.request.body}` with
`{"label":"x"}` passes the raw JSON through to SQLite — use
`json_extract(:body, '$.label')` in the query if you want the field.

### Positional

Names are `1`, `2`, … and the SQL uses `?`. Positional binds must be `1..N` in
order, and the count must match the number of `?` in the statement.

```caddyfile
sqlite {
	query  "INSERT INTO app.notes (body) VALUES (?)"
	bind   1 {http.request.body}
	output json
}
```

### Named

Any other name, with the SQL using `:name`, `@name` or `$name`. Order does not
matter, and the value reaches the driver as a named argument.

```caddyfile
sqlite {
	query  "UPDATE app.notes SET body = :body WHERE id = :id"
	bind   id   {http.request.uri.query.id}
	bind   body {http.request.body}
	output json
}
```

### Mixing

Rejected. The first bind's name shape determines the mode; a bind of the wrong
shape is a parse error.

### Validation

Named binds are checked against the SQL at `caddy validate`, in both directions,
because the driver only reports one of them and only at request time:

- a placeholder with no `bind` line → config error (`sql uses :id but no bind
  declares it`); at runtime the driver would say `missing named argument "id"` on
  every request;
- a `bind` line with no placeholder → config error (`bind "extra" is never used by
  the sql`); the driver silently ignores extra arguments, so this would otherwise
  look like it did something.

The scan ignores comments, string literals and quoted identifiers, so
`WHERE created_at = '12:30'` is not mistaken for a `:30` bind. Positional binds
are validated by count instead, which the driver also enforces.

### Values

**All resolved values are strings.** SQLite applies type affinity to string-bound
parameters. Cast in SQL when type matters: `CAST(? AS INTEGER)`, `strftime(?, ...)`.
This is not a limitation — it's what "no SQL interpolation" means. The value you
bind is the value SQLite sees.

**Escaping:** none required. Binds go to the driver as `database/sql` parameters;
the replacer substitution result is bound verbatim.

## Output formats

Set with `output`. Default `json`.

### `json`

```json
{"status": "ok", "data": {"rows": [...]}}
{"status": "ok", "data": {"affected": 1}}
{"status": "error", "message": "unresolved bind \"2\": {json.email}"}
```

### `xml`

```xml
<response status="ok"><rows><row><id>1</id></row></rows></response>
<error><message>unresolved bind "2": {json.email}</message></error>
```

### `csv`

Header row from column names, one row per record, `nil` → empty field. Error: blank
body, status set.

### `plain`

One field per line, blank line between rows. `nil` → empty line. Error: error text
as body, status set.

### `dynamic`

The statement must return **exactly one row** with these columns, matched by name
and in any order:

| Column | Type | Meaning |
|---|---|---|
| `content` | BLOB/TEXT | Response body. |
| `content_type` | TEXT | `Content-Type` header. |
| `http_code` | INTEGER | Response status. Optional; defaults to `200`. |

Any other column, a duplicate column, or a row count other than one is a `500`
with an empty body. Use it when the database should decide the response:

```caddyfile
sqlite {
	query  "SELECT CASE WHEN id IS NULL
	                  THEN json_object('error', 'not found')
	                  ELSE json_object('id', id, 'body', body)
	             END AS content,
	             'application/json' AS content_type,
	             CASE WHEN id IS NULL THEN 404 ELSE 200 END AS http_code
	        FROM (SELECT id, body FROM app.notes WHERE id = :id
	              UNION ALL SELECT NULL, NULL LIMIT 1)"
	bind   id {re.note.id}
	output dynamic
}
```

### `none`

Runs the statement and answers with just a status and no body — the natural
partner of `status 204` for deletes and writes you only want to acknowledge:

```caddyfile
sqlite {
	query  "DELETE FROM app.notes WHERE id = :id"
	bind   id {re.note.id}
	output none
	status 204
}
```

### Content-Type per format

| Format | Success | Error |
|---|---|---|
| `json` | `application/json` | `application/json` |
| `xml` | `application/xml` | `application/xml` |
| `csv` | `text/csv` | `text/csv` |
| `plain` | `text/plain; charset=utf-8` | `text/plain; charset=utf-8` |
| `dynamic` | from `content_type` | unset, empty body |
| `none` | unset, empty body | unset, empty body |

## Caching

- One LRU per process, keyed by `FNV64a(generation, sqlText, args)`.
- The generation counter is bumped by every commit, from any schema. A write to
  `media` invalidates cached reads of `app`, because a single query can join both.
- Results over 100 rows are not cached.
- Single-flight: N concurrent misses on the same key run one query, and the rest
  wait for its result.
- `cache 0` bypasses the cache for that handler only; it doesn't evict other
  callers' entries.
- Template reads share the same cache; template writes bump the generation.
- External writers (`sqlite3` CLI, cron) don't bump the generation — staleness is
  bounded by `cache_ttl`.

## Migrations

Per database, opt-in with `migrate <dir>`. Files are `NNNN_<name>.sql`; the
numeric prefix is the version and files without one are ignored.

At startup the engine reads `PRAGMA user_version` from that file and applies, in
numeric order, only the files whose version is **greater** than the stored value.
Each file runs in its own transaction together with the version update, so:

- Re-running Caddy is a no-op.
- A failure rolls the file's partial work back and leaves the version untouched —
  `Provision` errors and Caddy does not start.

**Forward-only.** No down migrations. Backups are the recovery path — which is why
`backup` exists.

## Backups

Opt-in per database via `backup`. Uses `VACUUM INTO`, which reads the source in a
single read transaction, handles WAL correctly, and writes a defragmented file
**without blocking the writer**. This is the right primitive for online backups of
a live SQLite database — `cp` of a live file is not.

```caddyfile
db media /srv/media.db {
	backup {
		schedule 0 3 * * *       # cron: 03:00 daily
		dir      /srv/backups
		retain   7               # keep 7 most recent; 0 = unlimited
	}
}
```

| Option | Notes |
|---|---|
| `method` | Optional; only `vacuum_into` is accepted. Anything else is rejected at parse time. |
| `schedule` | Cron expression — five fields (minute, hour, day-of-month, month, day-of-week) or a descriptor: `@daily`, `@hourly`, `@every 1h`. Minimum interval is 1s. |
| `dir` | Directory for backup files. Created if missing. |
| `retain` | Number of most-recent backups to keep. `0` keeps all. |

Each run writes `<name>-<timestamp>.db` in `dir`, where the timestamp is
`YYYYMMDD-HHMMSS.mmm`. **Does not overwrite an existing file** — if two runs land
in the same millisecond the second gets a numeric suffix, so a name is never
reused. Requires SQLite 3.27.0+.

**Schedule:** one goroutine per database with a cron scheduler. The backup runs
`VACUUM INTO` on its own connection, not through the batcher. A failed backup logs
a warning and retries on the next tick; it does not affect the request path.

**Retention:** `retain N` keeps the N most recent files matching `<name>-*.db` in
`dir` (newest by modification time), deleting older ones after a successful backup.

**Read-only databases** cannot be backed up: `backup` on a `readonly` database is a
config error at parse time.

**What this does not do:** it does not restore. Restoring is a file copy while
Caddy is stopped. Backups are for recovery, not hot-standby.

## Template function

Registered as `http.handlers.templates.functions.sqlite` for the `templates`
middleware. Positional arguments only, and **no database argument** — the schema in
the SQL says where the data lives.

```caddyfile
templates {
	extensions {
		sqlite
	}
}
```

```html
{{ range sqlite "SELECT name, email FROM app.users WHERE tenant = ? AND status = ?" .Tenant "active" }}
	<li>{{ .name }} — {{ .email }}</li>
{{ end }}
```

**Behavior:**

- Arg 1: SQL.
- Remaining args: bound positionally to `?`.
- Returns `[]map[string]any` — same shape as `sqlite` rows.

**Reads share the cache.** The same query with the same args, whether from a
handler or a template, hits the same LRU.

**Writes go through the batcher.** The SQL is classified the same way as a
handler; writes must name their schema, route through that schema's `WriteStmt`,
commit a batch, and bump the generation. `RETURNING` is supported. Non-RETURNING
writes return an empty slice, so templates don't have to special-case the result.

**Types:** template arguments bind verbatim — `int64` as `int64`, `string` as
`string`. Unlike a handler (where every value is a string from the replacer), the
template function preserves types. That's the right behavior for a Go-side API —
you have real typed values, pass them.

**Context:** the function uses `context.Background()`, bounded by the engine
`timeout`. Single-flight waiters are not cancelable by client disconnect.
Acceptable for template rendering.

## Optimize

```caddyfile
db app /var/lib/caddy/app.db {
	optimize          # bare: the default interval
	optimize 6h       # or an explicit one
	optimize 0        # off
}
```

**Bare `optimize` means once a day**, because that is SQLite's own guidance for
`PRAGMA optimize`: it is cheap, it does nothing unless a table's statistics are
stale enough to matter, and it is meant to run periodically rather than per query.
`optimize 0` turns it off; an explicit duration overrides the default.

What the module issues is a pair tuned to this engine's own defaults, not the bare
statement:

```sql
PRAGMA wal_checkpoint(PASSIVE);   -- fold the WAL back into the main file
PRAGMA analysis_limit=400;        -- bound how much of each index ANALYZE may read
PRAGMA optimize;                  -- ANALYZE only the tables whose stats are stale
```

Each choice follows from how the module is configured, and the numbers are
deliberate:

- **The checkpoint comes first** so the ANALYZE reads the main file instead of a
  WAL that has been accumulating since the last pass. It also makes cold reads
  cheaper and `VACUUM INTO` backups smaller.
- **`PASSIVE`, never `TRUNCATE`.** The WAL is already bounded by
  `wal_autocheckpoint=2000`, so the point is folding committed pages back into the
  main file, not shrinking a file that will regrow. `PASSIVE` cannot block readers
  or the writer; a busy result is not an error, and the next pass catches up.
- **`analysis_limit=400`, not SQLite's example 1000.** Reads here are cached and
  index-driven, so planner statistics matter mainly for the first miss after a
  cache window. A few hundred rows per index is already representative for the
  query shapes this module serves (equality and range lookups, small `ORDER BY`s),
  and it keeps the pass short while requests are in flight. A larger sample buys
  nothing the plans can use.
- **All three run on one borrowed connection**, because `analysis_limit` is
  connection state. Nothing is set globally, so the request path is unaffected.

Two more properties:

- **A final pass runs at shutdown.** SQLite recommends running `PRAGMA optimize`
  when a connection is about to close, so the module does exactly that for every
  database that opted in — the last thing before its maintenance handle closes.
- **Nothing is batched and nothing is invalidated.** It doesn't go through the
  batcher (a few statements, once a day) and doesn't bump the generation counter:
  there are no cacheable results to invalidate, since `sqlite_stat1` affects query
  planning, not query results.

**Do not put `PRAGMA optimize` in a handler** — at request rate it is a no-op with
per-request overhead. This sub-directive exists precisely to prevent that.

## Tuning reference

### DSN pragmas (set by the module; not configurable)

| Pragma | Value | Why |
|---|---|---|
| `journal_mode` | `WAL` | Readers don't block the writer. Skipped for read-only and `:memory:`. |
| `synchronous` | `NORMAL` | Durable enough with WAL, much faster than `FULL`. |
| `busy_timeout` | `5000` | Wait for locks instead of failing immediately. |
| `foreign_keys` | `ON` | Enforce FKs. Off by default in SQLite. |
| `cache_size` | `-32000` | 32 MiB page cache per connection. |
| `mmap_size` | `268435456` | 256 MiB memory-mapped I/O. Skipped for `:memory:`. |
| `temp_store` | `MEMORY` | Temp tables in RAM. |
| `wal_autocheckpoint` | `2000` | Checkpoint every ~8 MiB of WAL. |

Read-only opens set `mode=ro` and skip WAL/sync/mmap.

### Connection pool (set by the module)

One pool for all reads: `SetMaxOpenConns = SetMaxIdleConns = min(NumCPU, 8) * 2 + 1`,
`SetConnMaxLifetime(0)`. Every connection carries every declared database as an
attached schema, established when the connection is created (SQLite's `ATTACH` is
per-connection state), which costs roughly 50-135 µs per attached file once per
connection and nothing per query.

### Per-database knobs

`batch_size`, `batch_timeout` (writes), `migrate`, `readonly`, `optimize`,
`backup`. Writes are batched per database, so one busy schema does not delay
another's commits.

### Handler knobs

`cache 0` and `status` per handler; `timeout` and the cache size/TTL are
engine-wide.

## Reusable REST pattern

`www/rest.caddyfile` ships a full CRUD resource — list, create, fetch, update,
patch, delete with `201`/`404`/`204`/`405` responses. The image keeps it at
`/etc/caddy/rest.caddyfile`, outside the served root, and its demo config imports
that absolute path; a relative import resolves against the importing Caddyfile's
directory (`/srv/www` in the image), where the snippet no longer sits. The demo
config uses it for `/api/notes` and `/api/tags`. Import it at the top level,
declare the method matchers once per site, then instantiate it per resource:

```caddyfile
# the image's own config
import /etc/caddy/rest.caddyfile
```

Your own config can import an absolute path too, or a copy placed beside the
importing file (`import rest.caddyfile`). Then, per resource:

```caddyfile
example.com {
	@get    method GET
	@post   method POST
	@put    method PUT
	@patch  method PATCH
	@delete method DELETE

	import rest notes  app.notes    body
	import rest assets media.assets name
}
```

Arguments are `{args[0]}` resource name (path segment, matcher suffix and
`path_regexp` name), `{args[1]}` `schema.table`, `{args[2]}` the text column
exposed as `body`. They are substituted textually when the Caddyfile is read, so
they may name tables and columns; request data never reaches SQL that way, because
it arrives as a bind.

**Required table shape.** The snippet's queries reference three columns, so the
target table needs all three:

```sql
CREATE TABLE tags (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,                       -- {args[2]}, exposed as "body"
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

`id` is the path parameter and the lookup key, `{args[2]}` is the column the
request body writes to and the response returns as `body`, and `created_at` is
returned with every row. A missing column is a runtime SQLite error, not a config
error, so create the table (a migration is the natural place) before the first
request.

Two behaviours worth knowing: `DELETE` answers `204` even when no row matched, and
the `POST`/`PUT`/`PATCH` bodies are read as JSON with the field named after
`{args[2]}` (`{"name":"…"}` for the `tags` example) — a raw text body is not
accepted by the snippet.

## Full example

```caddyfile
{
	admin localhost:2019

	sqlite {
		batch_size    128
		batch_timeout 5ms
		cache_ttl     5s
		cache_max     1000
		timeout       30s

		db app /var/lib/caddy/app.db {
			migrate  /srv/www/migrations
			optimize 24h
		}

		db media /srv/media.db {
			migrate  /srv/www/media-migrations
			readonly
		}

		db main {
			migrate /srv/www/scratch-migrations
		}
	}
}

example.com {
	# Read, cached, joining two schemas.
	handle /api/notes {
		sqlite {
			query  "SELECT n.id, n.body, m.name AS asset
			        FROM app.notes n LEFT JOIN media.assets m ON m.id = n.asset_id
			        ORDER BY n.id DESC"
			output json
		}
	}

	# Write through app's batcher.
	handle /api/notes/add {
		sqlite {
			query  "INSERT INTO app.notes (body) VALUES (json_extract(:body, '$.body'))
			        RETURNING id, body, created_at"
			bind   body {http.request.body}
			output json
			status 201
		}
	}

	# The database calling out, inside the statement.
	handle /api/notes/sync {
		sqlite {
			query  "INSERT INTO app.notes (body) VALUES (http_get(json_extract(:body, '$.url')))"
			bind   body {http.request.body}
			output json
		}
	}

	# Scratch: per-process counters, gone on reload.
	handle /api/hit {
		sqlite {
			query  "INSERT INTO main.hits (path) VALUES (?) RETURNING id"
			bind   1 {http.request.uri.path}
			output json
		}
	}

	# Fire and forget, answered by the next handler.
	handle /api/events {
		sqlite {
			query    "INSERT INTO app.events (kind) VALUES (:kind)"
			bind     kind {http.request.body}
			continue
		}
		respond "queued" 202
	}
}
```

## Implementation notes

- The pool, batcher, cache, migrations, backups and classification live in
  `pkg/database`, which has **no Caddy imports** and is testable on its own.
- Only `pkg/plugin` imports Caddy: the global option parser, the handler, the
  template function, bind resolution and output serialization.
- Reads run on one pool whose connections carry every schema; writes route to the
  owning schema's batcher, which keeps every batch inside a single file and so
  keeps commits atomic.
- `test/unit.sh` runs `gofmt`, `go vet` and the unit tests in the builder image;
  it never touches a host Go toolchain.
- `test/e2e.sh` runs the built image and exercises every endpoint, output format,
  bind mode, the REST snippet, backups, and the provision-time rejections. It
  also starts the image with no overrides, so a broken default config or snippet
  location fails the suite instead of surfacing on the first `docker run`.

## Build

```sh
./build.sh                       # builds the image (tags <repo>:test)
sh test/unit.sh                  # gofmt, vet and unit tests in the builder image
sh test/e2e.sh                   # runs the end-to-end suite against that image
```

## Known caveats

1. **No `use`, and no unqualified tables.** Every query names its schema, and
   unqualified names land in the in-memory scratch. A forgotten qualifier is a
   runtime `no such table` for reads and a config error for writes.
2. **One write, one schema.** A statement that writes to two schemas is rejected,
   because SQLite cannot make a transaction across attached databases atomic in
   WAL mode.
3. **`readonly` needs write access once.** Migrations are applied on open, then the
   file is served `mode=ro`. A genuinely read-only mount fails at startup with a
   message saying so.
4. **The read cache is process-wide.** A write to any schema invalidates cached
   reads of all of them.
5. **The scratch schema is transient.** It lives in memory with the read pool and
   is empty after a reload.
6. **Errors go to logs, not responses.** Driver errors name tables and columns;
   clients get the format-appropriate error body. The one exception is a bind
   error, which the chain sees as an error rather than a body.
7. **`http_get`/`http_post` are SSRF-capable.** They fetch whatever URL is bound,
   with no allowlist, and a 5s client timeout. Treat user-controlled binds as
   untrusted hosts.
8. **CSV errors are blank-bodied; plain errors are the error text.** Clients must
   check the status code to distinguish success from failure.
9. **`dynamic` requires exactly one row.** Zero rows or more than one is a `500`
   with an empty body — use `UNION ALL … LIMIT 1` when a lookup can miss.
10. **The template function classifies SQL per call** rather than once at
    provision, because templates are evaluated at render time. Classification is
    sub-microsecond for typical statements.
11. **`backup` is for recovery, not hot-standby.** Restoring is a file copy while
    Caddy is stopped.
12. **`PRAGMA optimize` must not be a `sqlite` query.** Use the `optimize`
    sub-directive. At request rate, `PRAGMA optimize` is a no-op with per-request
    overhead; as a scheduled task, it's the SQLite-recommended maintenance pattern.

## Coming from the old syntax

| Old | New |
|---|---|
| `sqlite_db <name> <file> { … }` (global) | `sqlite { db <name> <file> { … } }` |
| `use <name>` | gone — name the schema in the SQL (`app.notes`, `main.hits`) |
| `sqlite_response { … }` | `sqlite { … }` |
| `sqlite_bg { … on_complete { url … } }` | `sqlite { … continue }`; no callback — use `http_get`/`http_post` inside the SQL when the database must call out |
| `{{ sqlite "<db>" "<sql>" }}` | `{{ sqlite "<sql>" }}` |
| per-database `cache_ttl` / `cache_max` | engine-wide `cache_ttl` / `cache_max` in the `sqlite { }` block |
| per-database `batch_size` / `batch_timeout` | unchanged (still per database) |
