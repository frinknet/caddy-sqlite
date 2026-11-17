#!/bin/sh
# e2e test runner — runs Caddy in Docker, exercises every endpoint, checks results.
set -eu

TMP=$(mktemp -d /tmp/caddy-e2e-XXXXXX)
PORT=9099
IMAGE="${E2E_IMAGE:-caddysqlite-e2e}"
CONTAINER_NAME="caddysqlite-e2e-runner"
PASS=0
FAIL=0

cleanup() {
	docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

# ── Migrations for each declared database ──────────────
mkdir -p "$TMP/migrations" "$TMP/extra-migrations" "$TMP/ro-migrations" "$TMP/bk-migrations" "$TMP/scratch-migrations"

cat > "$TMP/migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS items (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    label  TEXT,
    tag_id INTEGER
);
SQL

cat > "$TMP/extra-migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS tags (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
SQL

# This database is declared readonly: the engine migrates it once on open and
# then serves it mode=ro, so the seed row proves the migration ran.
cat > "$TMP/ro-migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS snapshots (id INTEGER PRIMARY KEY, label TEXT);
INSERT INTO snapshots (label) VALUES ('seed');
SQL

cat > "$TMP/scratch-migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS hits (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT);
SQL

cat > "$TMP/bk-migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS bk_items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT);
SQL

# ── Template file for the {{ sqlite }} function ────────
mkdir -p "$TMP/tpl"
cat > "$TMP/tpl/index.html" << 'TPL'
{{ range sqlite "SELECT id, label FROM e2e.items WHERE label = 'first'" }}{{ .label }}{{ end }}
TPL

# ── Reusable REST snippet under test ───────────────────
mkdir -p "$TMP/snippets"
cp www/rest.caddyfile "$TMP/snippets/rest.caddyfile"

# Pre-create the backup dir so it stays host-owned: the container writes
# root-owned files into it, and only a writable directory lets the user
# delete them again at cleanup.
mkdir -p "$TMP/backups"

# ── Create test Caddyfile ──────────────────────────────
cat > "$TMP/Caddyfile" << 'CADDY'
{
	admin off
	debug

	sqlite {
		batch_size    8
		batch_timeout 2ms
		cache_ttl     5s
		cache_max     100
		timeout       30s

		db e2e /data/test.db {
			migrate  /data/migrations
			optimize 1h
		}

		db extra /data/extra.db {
			migrate /data/extra-migrations
		}

		db ro /data/ro.db {
			migrate  /data/ro-migrations
			readonly
		}

		db main {
			migrate /data/scratch-migrations
		}

		db bk /data/bk.db {
			migrate /data/bk-migrations
			backup {
				schedule @every 1s
				dir      /data/backups
				retain   2
			}
		}
	}
}

# Snippets are defined at the top level, outside any site block.
import /data/snippets/rest.caddyfile

:80 {
	# The REST snippet needs the method matchers declared once per site.
	@get    method GET
	@post   method POST
	@put    method PUT
	@patch  method PATCH
	@delete method DELETE

	import rest tags extra.tags name

	# DDL through a handler: writes must name their schema, and output none
	# answers with a bare status.
	route /setup {
		sqlite {
			query  "CREATE TABLE IF NOT EXISTS e2e.items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT, tag_id INTEGER)"
			output none
			status 204
		}
	}

	# Positional binds
	route /insert {
		sqlite {
			query  "INSERT INTO e2e.items (label) VALUES (json_extract(?, '$.label'))"
			bind   1 {http.request.body}
			output json
		}
	}

	# Named binds
	# SQLite schema names are case-insensitive; the module resolves them to the
	# declared spelling.
	route /case-insert {
		sqlite {
			query  "INSERT INTO E2E.items (label) VALUES (json_extract(?, '$.label'))"
			bind   1 {http.request.body}
			output json
		}
	}

	route /named-insert {
		sqlite {
			query  "INSERT INTO e2e.items (label, tag_id) VALUES (json_extract(:body, '$.label'), :tag) RETURNING id, label"
			bind   body {http.request.body}
			bind   tag  {http.request.uri.query.tag}
			output json
			status 201
		}
	}

	# Cached read
	route /items {
		sqlite {
			query  "SELECT id, label FROM e2e.items ORDER BY id"
			output json
		}
	}

	# Cross-schema join: items live in e2e, tags in extra
	route /joined {
		sqlite {
			query  "SELECT i.id, i.label, t.name AS tag
			        FROM e2e.items i LEFT JOIN extra.tags t ON t.id = i.tag_id
			        ORDER BY i.id"
			output json
		}
	}

	route /count {
		sqlite {
			query  "SELECT COUNT(*) AS cnt FROM e2e.items"
			output json
		}
	}

	# Read-only schema, migrated on open then frozen
	route /ro {
		sqlite {
			query  "SELECT id, label FROM ro.snapshots ORDER BY id"
			output json
		}
	}

	# In-memory scratch
	route /scratch-hit {
		sqlite {
			query  "INSERT INTO main.hits (path) VALUES (?) RETURNING id"
			bind   1 {http.request.uri.path}
			output json
		}
	}

	route /scratch-count {
		sqlite {
			query  "SELECT COUNT(*) AS hits FROM main.hits"
			output json
		}
	}

	# Fire-and-forget: no response here, the next handler answers
	route /queued {
		sqlite {
			query    "INSERT INTO e2e.items (label) VALUES (json_extract(:body, '$.label'))"
			bind     body {http.request.body}
			continue
		}
		respond "queued" 202
	}

	# Status codes
	route /insert-returning {
		sqlite {
			query  "INSERT INTO e2e.items (label) VALUES (json_extract(:body, '$.label')) RETURNING id, label"
			bind   body {http.request.body}
			output json
			status 201
		}
	}

	route /health {
		sqlite {
			query  "SELECT 'alive' AS health"
			output json
		}
	}

	# UDFs
	route /http-get {
		sqlite {
			query  "SELECT http_get(json_extract(?, '$.url')) AS result"
			bind   1 {http.request.body}
			output json
		}
	}

	route /http-post {
		sqlite {
			query  "SELECT http_post(json_extract(?, '$.url'), json_extract(?, '$.data')) AS result"
			bind   1 {http.request.body}
			bind   2 {http.request.body}
			output json
		}
	}

	# Output formats
	route /dynamic {
		sqlite {
			query  "SELECT 'text/plain' AS content_type, 'from-dynamic' AS content, 201 AS http_code"
			output dynamic
		}
	}

	route /items-xml {
		sqlite {
			query  "SELECT id, label FROM e2e.items ORDER BY id"
			output xml
		}
	}

	route /items-plain {
		sqlite {
			query  "SELECT id, label FROM e2e.items ORDER BY id"
			output plain
		}
	}

	route /items-csv {
		sqlite {
			query  "SELECT id, label FROM e2e.items ORDER BY id"
			output csv
		}
	}

	route /backed-up {
		sqlite {
			query  "INSERT INTO bk.bk_items (label) VALUES (json_extract(?, '$.label'))"
			bind   1 {http.request.body}
			output json
		}
	}

	# {{ sqlite }} template function
	handle_path /tpl/* {
		templates {
			extensions {
				sqlite
			}
		}
		file_server {
			root /data/tpl
		}
	}
}
CADDY

# ── Test 0: the image runs the versions go.mod pins ────
# The container and `go test` must resolve the same dependencies: a newer
# driver in the image is exactly how user-defined functions disappeared once.
echo ""
echo "=== Test 0: dependency versions ==="
for mod in "modernc.org/sqlite" "github.com/caddyserver/caddy/v2"; do
	want=$(awk -v m="$mod" '$1 == m && $2 ~ /^v/ {print $2}' go.mod)
	got=$(docker run --rm --entrypoint /bin/cat "$IMAGE" /usr/share/licenses/versions.txt 2>/dev/null \
		| awk -v m="$mod" '$1 == "dep" && $2 == m {print $3}')
	if [ -n "$want" ] && [ "$want" = "$got" ]; then
		echo "  PASS: $mod $got matches go.mod"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $mod: go.mod pins ${want:-nothing}, image has ${got:-nothing}"
		FAIL=$((FAIL + 1))
	fi
done

echo "=== Starting Caddy container ==="
docker run -d \
	--name "$CONTAINER_NAME" \
	-v "$TMP/Caddyfile:/etc/caddy/Caddyfile:ro" \
	-v "$TMP:/data" \
	-p "$PORT:80" \
	"$IMAGE" \
	run --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1

echo "=== Waiting for Caddy ==="
for i in 1 2 3 4 5 6 7 8 9 10; do
	if curl -sf http://127.0.0.1:$PORT/health >/dev/null 2>&1; then
		echo "Caddy ready after ${i}s"
		break
	fi
	sleep 1
done

check() {
	local desc="$1" expected="$2" actual="$3"
	if echo "$actual" | grep -q "$expected"; then
		echo "  PASS: $desc"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $desc"
		echo "    expected to contain: $expected"
		echo "    got: $actual"
		FAIL=$((FAIL + 1))
	fi
}

# ── Test 1: DDL through a handler ──────────────────────
echo ""
echo "=== Test 1: setup ==="
RESP=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/setup)
check "setup returns 204" '204' "$RESP"

# ── Test 2: positional binds ───────────────────────────
echo ""
echo "=== Test 2: positional binds ==="
RESP=$(curl -sf -X POST -d '{"label":"first"}' http://127.0.0.1:$PORT/insert)
check "insert first" '"affected":1' "$RESP"
RESP=$(curl -sf -X POST -d '{"label":"second"}' http://127.0.0.1:$PORT/insert)
check "insert second" '"affected":1' "$RESP"

# ── Test 3: cached read ────────────────────────────────
echo ""
echo "=== Test 3: cached read ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/items)
check "rows returned" '"rows":' "$RESP"
check "first row present" '"first"' "$RESP"
check "second row present" '"second"' "$RESP"

RESP=$(curl -sf http://127.0.0.1:$PORT/count)
check "count is 2" '"cnt":2' "$RESP"

# ── Test 4: named binds + status code ──────────────────
echo ""
echo "=== Test 4: named binds ==="
RESP=$(curl -sS -o /tmp/e2e-named.json -w '%{http_code}' -X POST \
	-d '{"label":"named"}' "http://127.0.0.1:$PORT/named-insert?tag=1")
check "named insert returns 201" '201' "$RESP"
check "named insert body" '"named"' "$(cat /tmp/e2e-named.json)"
rm -f /tmp/e2e-named.json

# ── Test 4b: case-insensitive schema ───────────────────
RESP=$(curl -sf -X POST -d '{"label":"cased"}' http://127.0.0.1:$PORT/case-insert)
check "case-variant schema writes" '"affected":1' "$RESP"
RESP=$(curl -sf http://127.0.0.1:$PORT/items)
check "row landed in the declared schema" '"cased"' "$RESP"

# ── Test 5: cross-schema join ──────────────────────────
echo ""
echo "=== Test 5: cross-schema join ==="
RESP=$(curl -sS -X POST -d '{"name":"blue"}' http://127.0.0.1:$PORT/api/tags)
check "tag created via REST snippet" '"blue"' "$RESP"
RESP=$(curl -sf http://127.0.0.1:$PORT/joined)
check "join returns the tag name" '"blue"' "$RESP"

# ── Test 6: read-only schema ───────────────────────────
echo ""
echo "=== Test 6: read-only schema ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/ro)
check "read-only schema was migrated on open" '"seed"' "$RESP"

# ── Test 7: in-memory scratch ──────────────────────────
echo ""
echo "=== Test 7: scratch ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/scratch-hit)
check "scratch write returns an id" '"id":' "$RESP"
RESP=$(curl -sf http://127.0.0.1:$PORT/scratch-count)
check "scratch read sees it" '"hits":1' "$RESP"

# ── Test 8: continue passes the request on ─────────────
echo ""
echo "=== Test 8: continue ==="
RESP=$(curl -sS -o /dev/null -w '%{http_code}' -X POST -d '{"label":"bg"}' http://127.0.0.1:$PORT/queued)
check "next handler answers" '202' "$RESP"
sleep 1
RESP=$(curl -sf http://127.0.0.1:$PORT/items)
check "background write landed" '"bg"' "$RESP"

# ── Test 9: UDFs ───────────────────────────────────────
echo ""
echo "=== Test 9: UDFs ==="
RESP=$(curl -sf -X POST -d '{"url":"http://127.0.0.1:80/health"}' http://127.0.0.1:$PORT/http-get)
check "http_get returns the health body" 'alive' "$RESP"
RESP=$(curl -sf -X POST -d '{"url":"http://127.0.0.1:80/health","data":"{}"}' http://127.0.0.1:$PORT/http-post)
check "http_post returns the health body" 'alive' "$RESP"

# ── Test 10: output formats ────────────────────────────
echo ""
echo "=== Test 10: output formats ==="
RESP=$(curl -sS -i http://127.0.0.1:$PORT/dynamic)
check "dynamic status" '201' "$RESP"
check "dynamic content type" 'Content-Type: text/plain' "$RESP"
check "dynamic body" 'from-dynamic' "$RESP"

RESP=$(curl -sf http://127.0.0.1:$PORT/items-xml)
check "xml rows" '<rows>' "$RESP"
check "xml row data" '<label>first</label>' "$RESP"

RESP=$(curl -sf http://127.0.0.1:$PORT/items-plain)
check "plain text" 'first' "$RESP"

RESP=$(curl -sf http://127.0.0.1:$PORT/items-csv)
check "csv header" 'id,label' "$RESP"
check "csv data" '1,first' "$RESP"

# ── Test 11: template function ─────────────────────────
echo ""
echo "=== Test 11: template function ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/tpl/index.html)
check "template ran sqlite" 'first' "$RESP"

# ── Test 12: REST snippet ──────────────────────────────
echo ""
echo "=== Test 12: REST snippet ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/api/tags)
check "list" '"rows":' "$RESP"

RESP=$(curl -sS -X POST -d '{"name":"red"}' http://127.0.0.1:$PORT/api/tags)
check "create returns the row" '"red"' "$RESP"
ID=$(echo "$RESP" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
check "created row has an id" '^[0-9]' "$ID"

RESP=$(curl -sf http://127.0.0.1:$PORT/api/tags/$ID)
check "fetch returns the row" '"red"' "$RESP"

RESP=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/api/tags/99999)
check "missing row returns 404" '404' "$RESP"

RESP=$(curl -sf -X PATCH -d '{"name":"crimson"}' http://127.0.0.1:$PORT/api/tags/$ID)
check "patch updates the row" '"crimson"' "$RESP"

RESP=$(curl -sf -X PUT -d '{"name":"scarlet"}' http://127.0.0.1:$PORT/api/tags/$ID)
check "put replaces the row" '"scarlet"' "$RESP"

RESP=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE http://127.0.0.1:$PORT/api/tags/$ID)
check "delete returns 204" '204' "$RESP"

RESP=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/api/tags/$ID)
check "deleted row is gone" '404' "$RESP"

RESP=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT http://127.0.0.1:$PORT/api/tags)
check "unmatched method returns 405" '405' "$RESP"

# ── Test 13: scheduled backup ──────────────────────────
echo ""
echo "=== Test 13: scheduled backup ==="
BK_COUNT=0
for i in 1 2 3 4 5 6 7 8 9 10; do
	BK_COUNT=$(ls "$TMP/backups" 2>/dev/null | grep -c '^bk-.*\.db$' || true)
	if [ "$BK_COUNT" -ge 1 ]; then
		break
	fi
	sleep 0.5
done

RESP=$(curl -sf -X POST -d '{"label":"bk-seed"}' http://127.0.0.1:$PORT/backed-up)
check "backed-up database accepts writes" '"affected":1' "$RESP"
sleep 2.5

NEWEST=$(ls -t "$TMP/backups" 2>/dev/null | grep '^bk-.*\.db$' | head -1)
BK_COUNT=$(ls "$TMP/backups" 2>/dev/null | grep -c '^bk-.*\.db$' || true)
echo "  backups on disk: $BK_COUNT (newest: ${NEWEST:-none})"
check "backup file written" 'bk-' "$NEWEST"

if [ -n "$NEWEST" ] && grep -qa 'SQLite format 3' "$TMP/backups/$NEWEST"; then
	echo "  PASS: backup is a SQLite database"
	PASS=$((PASS + 1))
else
	echo "  FAIL: backup is not a SQLite database"
	FAIL=$((FAIL + 1))
fi

if [ -n "$NEWEST" ] && grep -qa 'bk-seed' "$TMP/backups/$NEWEST"; then
	echo "  PASS: backup contains committed rows"
	PASS=$((PASS + 1))
else
	echo "  FAIL: backup does not contain committed rows"
	FAIL=$((FAIL + 1))
fi

if [ "$BK_COUNT" -le 2 ]; then
	echo "  PASS: retention keeps at most 2 backups"
	PASS=$((PASS + 1))
else
	echo "  FAIL: retention kept $BK_COUNT backups"
	FAIL=$((FAIL + 1))
fi

if [ "$BK_COUNT" -ge 2 ]; then
	echo "  PASS: backups repeat on schedule"
	PASS=$((PASS + 1))
else
	echo "  FAIL: expected a second scheduled backup, saw $BK_COUNT"
	FAIL=$((FAIL + 1))
fi

# ── Test 14: provision-time validation ─────────────────
echo ""
echo "=== Provision-time validation ==="

neg() {
	local desc="$1" file="$2" expect="$3"
	local out status
	set +e
	out=$(docker run --rm -v "$file:/etc/caddy/Caddyfile:ro" -v "$TMP:/data" "$IMAGE" \
		validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1)
	status=$?
	set -e
	if [ "$status" -ne 0 ] && echo "$out" | grep -q "$expect"; then
		echo "  PASS: $desc"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $desc"
		echo "    expected non-zero exit containing: $expect"
		echo "    got exit=$status: $out"
		FAIL=$((FAIL + 1))
	fi
}

cat > "$TMP/neg-unqualified.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db
	}
}
:8080 {
	route /x {
		sqlite {
			query  "INSERT INTO items (label) VALUES ('x')"
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-readonly-write.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db ro /data/ro.db {
			readonly
		}
	}
}
:8080 {
	route /x {
		sqlite {
			query  "INSERT INTO ro.snapshots (label) VALUES ('x')"
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-unknown-schema.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db
	}
}
:8080 {
	route /x {
		sqlite {
			query  "INSERT INTO nope.t (x) VALUES (1)"
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-two-schemas.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app   /data/test.db
		db extra /data/extra.db
	}
}
:8080 {
	route /x {
		sqlite {
			query  "INSERT INTO app.items (id) SELECT id FROM extra.tags; DELETE FROM extra.tags"
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-readonly-backup.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db ro /data/ro.db {
			readonly
			backup {
				schedule @daily
				dir      /data/backups
			}
		}
	}
}
CADDY

cat > "$TMP/neg-bind-missing.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db
	}
}
:8080 {
	route /x {
		sqlite {
			query "UPDATE app.items SET label = :label WHERE id = :id"
			bind  label {http.request.body}
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-bind-unused.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db
	}
}
:8080 {
	route /x {
		sqlite {
			query "UPDATE app.items SET label = :label WHERE id = :id"
			bind  label {http.request.body}
			bind  id    {http.request.uri.query.id}
			bind  extra {http.request.uri.query.extra}
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-use.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db
	}
}
:8080 {
	route /x {
		sqlite {
			use    app
			query  "SELECT 1"
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-env.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db envtest /data/{env.CADDYSQLITE_E2E_UNSET}.db
	}
}
:8080 {
	respond "ok"
}
CADDY

cat > "$TMP/neg-method.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db {
			backup {
				method   cp
				schedule @daily
				dir      /data/backups
			}
		}
	}
}
CADDY

cat > "$TMP/neg-continue-output.Caddyfile" << 'CADDY'
{
	admin off
	sqlite {
		db app /data/test.db
	}
}
:8080 {
	route /x {
		sqlite {
			query    "INSERT INTO app.items (label) VALUES ('x')"
			output   json
			continue
		}
		respond "ok"
	}
}
CADDY

neg "unqualified write fails" "$TMP/neg-unqualified.Caddyfile" 'must name a schema'
neg "write to read-only schema fails" "$TMP/neg-readonly-write.Caddyfile" 'read-only database'
neg "unknown schema fails" "$TMP/neg-unknown-schema.Caddyfile" 'unknown database'
neg "write naming two schemas fails" "$TMP/neg-two-schemas.Caddyfile" 'one schema'
neg "readonly + backup fails" "$TMP/neg-readonly-backup.Caddyfile" 'cannot be used with read_only'
neg "use is gone" "$TMP/neg-use.Caddyfile" 'unknown option'
neg "placeholder with no bind fails" "$TMP/neg-bind-missing.Caddyfile" 'no bind declares it'
neg "unused bind fails" "$TMP/neg-bind-unused.Caddyfile" 'never used by the sql'
neg "missing env var fails" "$TMP/neg-env.Caddyfile" 'CADDYSQLITE_E2E_UNSET'
neg "unsupported backup method fails" "$TMP/neg-method.Caddyfile" 'unsupported method'
neg "continue with output fails" "$TMP/neg-continue-output.Caddyfile" 'cannot be combined with output'

# ── Summary ────────────────────────────────────────────
echo ""
echo "=========================================="
echo "  PASS: $PASS   FAIL: $FAIL"
echo "=========================================="
[ "$FAIL" -eq 0 ] && echo "ALL TESTS PASSED" || echo "SOME TESTS FAILED"
exit $FAIL
