#!/bin/sh
# e2e test runner — runs Caddy in Docker, exercises every endpoint, checks results.
set -eu

TMP=$(mktemp -d /tmp/caddy-e2e-XXXXXX)
DB="$TMP/test.db"
CERT="$TMP/cert.pem"
KEY="$TMP/key.pem"
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

# ── Seed schema via migration ──────────────────────────
# Statements are prepared at provision time, so every table referenced by
# a handler must exist before Caddy starts. The migration runs on open.
mkdir -p "$TMP/migrations"
cat > "$TMP/migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS items (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    label TEXT
);
SQL

# ── Template file for the {{ sqlite }} function ────────
mkdir -p "$TMP/tpl"
cat > "$TMP/tpl/index.html" << 'TPL'
{{ sqlite "e2e" "SELECT id, label FROM items WHERE label = 'first'" }}
TPL

# ── Seed the read-only DB fixture ──────────────────────
# `validate` provisions the app (open + migrate) and exits, leaving the
# file behind for the read_only database in the main config.
mkdir -p "$TMP/ro-migrations"
cat > "$TMP/ro-migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS ro_items (id INTEGER PRIMARY KEY, label TEXT);
INSERT INTO ro_items (label) VALUES ('seed');
SQL

mkdir -p "$TMP/bk-migrations"
cat > "$TMP/bk-migrations/0001_init.sql" << 'SQL'
CREATE TABLE IF NOT EXISTS bk_items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT);
SQL

# Pre-create the backup dir so it stays host-owned: the container writes
# root-owned files into it, and only a writable directory lets the user
# delete them again at cleanup.
mkdir -p "$TMP/backups"
cat > "$TMP/seed-ro.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db seed /data/ro.db {
		migrate /data/ro-migrations
	}
}
CADDY
docker run --rm -v "$TMP:/data" -v "$TMP/seed-ro.Caddyfile:/etc/caddy/Caddyfile:ro" "$IMAGE" \
	validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || true

# ── Create test Caddyfile ──────────────────────────────
cat > "$TMP/Caddyfile" << 'CADDY'
{
	admin off
	debug

	sqlite_db e2e /data/test.db {
		batch_size    8
		batch_timeout 2ms
		cache_ttl     5s
		cache_max     100
		migrate       /data/migrations
		optimize      1h
	}

	sqlite_db ro /data/ro.db {
		read_only
	}

	sqlite_db bk /data/bk.db {
		migrate /data/bk-migrations
		backup {
			schedule @every 1s
			dir      /data/backups
			retain   2
		}
	}
}

:80 {
# Create schema
route /setup {
	sqlite_response {
		use    e2e
		query  "CREATE TABLE IF NOT EXISTS items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)"
		output json
		cache  0
	}
}

# Insert — write through batcher
route /insert {
	sqlite_response {
		use    e2e
		query  "INSERT INTO items (label) VALUES (json_extract(?, '$.label'))"
		bind   1 {http.request.body}
		output json
	}
}

# Read all — cached read
route /items {
	sqlite_response {
		use    e2e
		query  "SELECT id, label FROM items ORDER BY id"
		output json
	}
}

# Count — cached read
route /count {
	sqlite_response {
		use    e2e
		query  "SELECT COUNT(*) AS cnt FROM items"
		output json
	}
}

# Insert RETURNING
route /insert-returning {
	sqlite_response {
		use    e2e
		query  "INSERT INTO items (label) VALUES (json_extract(?, '$.label')) RETURNING id, label"
		bind   1 {http.request.body}
		output json
	}
}

# Health endpoint the UDF routes below call back into
route /health {
	sqlite_response {
		use    e2e
		query  "SELECT 'alive' AS health"
		output json
	}
}

# http_get UDF
route /http-get {
	sqlite_response {
		use    e2e
		query  "SELECT http_get(json_extract(?, '$.url')) AS result"
		bind   1 {http.request.body}
		output json
	}
}

# http_post UDF
route /http-post {
	sqlite_response {
		use    e2e
		query  "SELECT http_post(json_extract(?, '$.url'), json_extract(?, '$.data')) AS result"
		bind   1 {http.request.body}
		bind   2 {http.request.body}
		output json
	}
}

# XML output
route /items-xml {
	sqlite_response {
		use    e2e
		query  "SELECT id, label FROM items ORDER BY id"
		output xml
	}
}

# Plain output
route /items-plain {
	sqlite_response {
		use    e2e
		query  "SELECT id, label FROM items ORDER BY id"
		output plain
	}
}

# CSV output
route /items-csv {
	sqlite_response {
		use    e2e
		query  "SELECT id, label FROM items ORDER BY id"
		output csv
	}
}

# output=dynamic, columns deliberately out of order
route /dynamic {
	sqlite_response {
		use    e2e
		query  "SELECT 'text/plain' AS content_type, 'from-dynamic' AS content, 201 AS http_code"
		output dynamic
	}
}

# sqlite_bg: async write, 202 + job id
route /bg-insert {
	sqlite_bg {
		use    e2e
		query  "INSERT INTO items (label) VALUES (json_extract(?, '$.label'))"
		bind   1 {http.request.body}
	}
}

# sqlite_bg with a webhook that records its own delivery
route /bg-webhook {
	sqlite_bg {
		use    e2e
		query  "INSERT INTO items (label) VALUES ('bg-hook')"
		on_complete {
			url    http://127.0.0.1:80/webhook-hit
			method POST
		}
	}
}

route /webhook-hit {
	sqlite_response {
		use    e2e
		query  "INSERT INTO items (label) VALUES ('webhook-fired')"
		output json
		cache  0
	}
}

# Read-only DB
route /ro {
	sqlite_response {
		use    ro
		query  "SELECT id, label FROM ro_items ORDER BY id"
		output json
	}
}

# Backed-up DB: writes land in every VACUUM INTO snapshot
route /bk-insert {
	sqlite_response {
		use    bk
		query  "INSERT INTO bk_items (label) VALUES (json_extract(?, '$.label'))"
		bind   1 {http.request.body}
		output json
		cache  0
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

echo "=== Starting Caddy container ==="
docker run -d \
	--name "$CONTAINER_NAME" \
	-v "$TMP/Caddyfile:/etc/caddy/Caddyfile:ro" \
	-v "$TMP:/data" \
	-p "$PORT:80" \
	"$IMAGE" \
	run --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1

# Wait for Caddy to be ready
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

# ── Test 1: Setup schema ────────────────────────────────
echo ""
echo "=== Test 1: Setup schema ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/setup)
check "setup creates table" '"status":"ok"' "$RESP"

# ── Test 2: Insert first item ───────────────────────────
echo ""
echo "=== Test 2: Insert item 1 ==="
RESP=$(curl -sf -X POST -d '{"label":"first"}' http://127.0.0.1:$PORT/insert)
check "insert first" '"affected":1' "$RESP"

# ── Test 3: Insert second item ──────────────────────────
echo ""
echo "=== Test 3: Insert item 2 ==="
RESP=$(curl -sf -X POST -d '{"label":"second"}' http://127.0.0.1:$PORT/insert)
check "insert second" '"affected":1' "$RESP"

# ── Test 4: Read all items ─────────────────────────────
echo ""
echo "=== Test 4: Read all items ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/items)
check "read items returns 2" '"rows":' "$RESP"
check "read items has first" '"first"' "$RESP"
check "read items has second" '"second"' "$RESP"

# ── Test 5: Count ──────────────────────────────────────
echo ""
echo "=== Test 5: Count ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/count)
check "count is 2" '"cnt":2' "$RESP"

# ── Test 6: Insert RETURNING ───────────────────────────
echo ""
echo "=== Test 6: Insert RETURNING ==="
RESP=$(curl -sf -X POST -d '{"label":"third"}' http://127.0.0.1:$PORT/insert-returning)
check "insert returning has rows" '"rows":' "$RESP"
check "insert returning has id 3" '"id":3' "$RESP"
check "insert returning has label" '"third"' "$RESP"

# ── Test 7: http_get UDF ──────────────────────────────
echo ""
echo "=== Test 7: http_get UDF ==="
RESP=$(curl -sf -X POST -d '{"url":"http://127.0.0.1:80/health"}' http://127.0.0.1:$PORT/http-get)
check "http_get returns health body" 'alive' "$RESP"

# ── Test 8: http_post UDF ─────────────────────────────
echo ""
echo "=== Test 8: http_post UDF ==="
RESP=$(curl -sf -X POST -d '{"url":"http://127.0.0.1:80/health","data":"{}"}' http://127.0.0.1:$PORT/http-post)
check "http_post returns health body" 'alive' "$RESP"

# ── Test 9: XML output ─────────────────────────────────
echo ""
echo "=== Test 9: XML output ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/items-xml)
check "xml response" '<response' "$RESP"
check "xml rows" '<rows>' "$RESP"
check "xml row data" '<label>first</label>' "$RESP"

# ── Test 10: Plain output ──────────────────────────────
echo ""
echo "=== Test 10: Plain output ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/items-plain)
check "plain text" 'first' "$RESP"
check "plain text" 'second' "$RESP"
check "plain text" 'third' "$RESP"

# ── Test 11: CSV output ───────────────────────────────
echo ""
echo "=== Test 11: CSV output ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/items-csv)
check "csv header" 'id,label' "$RESP"
check "csv data" '1,first' "$RESP"

# ── Test 12: dynamic output ────────────────────────────
echo ""
echo "=== Test 12: dynamic output ==="
RESP=$(curl -sS -i http://127.0.0.1:$PORT/dynamic)
check "dynamic status 201" '201' "$RESP"
check "dynamic content type" 'Content-Type: text/plain' "$RESP"
check "dynamic body" 'from-dynamic' "$RESP"

# ── Test 13: sqlite_bg async write ─────────────────────
echo ""
echo "=== Test 13: sqlite_bg ==="
RESP=$(curl -sS -X POST -d '{"label":"bg"}' http://127.0.0.1:$PORT/bg-insert)
check "bg accepted" '"status":"accepted"' "$RESP"
check "bg job id" '"job_id"' "$RESP"
sleep 1
RESP=$(curl -sf http://127.0.0.1:$PORT/items)
check "bg write landed" '"bg"' "$RESP"

# ── Test 14: {{ sqlite }} template function ────────────
echo ""
echo "=== Test 14: template function ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/tpl/index.html)
check "template ran sqlite" 'first' "$RESP"

# ── Test 15: read_only DB ──────────────────────────────
echo ""
echo "=== Test 15: read_only DB ==="
RESP=$(curl -sf http://127.0.0.1:$PORT/ro)
check "read-only DB serves rows" '"seed"' "$RESP"

# ── Test 16: sqlite_bg webhook ─────────────────────────
echo ""
echo "=== Test 16: sqlite_bg webhook ==="
RESP=$(curl -sS -X POST http://127.0.0.1:$PORT/bg-webhook)
check "bg webhook accepted" '"status":"accepted"' "$RESP"
sleep 1
RESP=$(curl -sf http://127.0.0.1:$PORT/items)
check "bg job write landed" '"bg-hook"' "$RESP"
check "webhook fired" '"webhook-fired"' "$RESP"

# ── Test 17: scheduled backup ──────────────────────────
echo ""
echo "=== Test 17: scheduled backup ==="
BK_COUNT=0
for i in 1 2 3 4 5 6 7 8 9 10; do
	BK_COUNT=$(ls "$TMP/backups" 2>/dev/null | grep -c '^bk-.*\.db$' || true)
	if [ "$BK_COUNT" -ge 1 ]; then
		break
	fi
	sleep 0.5
done

RESP=$(curl -sS -X POST -d '{"label":"bk-seed"}' http://127.0.0.1:$PORT/bk-insert)
check "backed-up DB accepts writes" '"affected":1' "$RESP"
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

# ── Tests 18+: provision-time validation ───────────────
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

cat > "$TMP/neg-ro-migrate.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db ro /data/ro.db {
		read_only
		migrate /data/migrations
	}
}
CADDY

cat > "$TMP/neg-ro-backup.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db ro /data/ro.db {
		read_only
		backup {
			schedule * * * * *
			dir      /data/backups
		}
	}
}
CADDY

cat > "$TMP/neg-ro-optimize.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db ro /data/ro.db {
		read_only
		optimize 1h
	}
}
CADDY

cat > "$TMP/neg-env.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db envtest /data/{env.CADDYSQLITE_E2E_UNSET}.db
}
:8080 {
	respond "ok"
}
CADDY

cat > "$TMP/neg-alias.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db e2e /data/test.db
}
:8080 {
	route /x {
		sqlite_response {
			use    nope
			query  "SELECT COUNT(*) AS n FROM items"
			output json
		}
	}
}
CADDY

cat > "$TMP/neg-method.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db e2e /data/test.db {
		backup {
			method   cp
			schedule * * * * *
			dir      /data/backups
		}
	}
}
CADDY

cat > "$TMP/neg-ro-write.Caddyfile" << 'CADDY'
{
	admin off
	sqlite_db ro /data/ro.db {
		read_only
	}
}
:8080 {
	route /x {
		sqlite_response {
			use    ro
			query  "INSERT INTO ro_items (label) VALUES ('nope')"
			output json
		}
	}
}
CADDY

neg "read_only + migrate fails" "$TMP/neg-ro-migrate.Caddyfile" 'migrate cannot be used with read_only'
neg "read_only + backup fails" "$TMP/neg-ro-backup.Caddyfile" 'backup cannot be used with read_only'
neg "read_only + optimize fails" "$TMP/neg-ro-optimize.Caddyfile" 'optimize cannot be used with read_only'
neg "write to read_only fails" "$TMP/neg-ro-write.Caddyfile" 'read-only'
neg "missing env var fails" "$TMP/neg-env.Caddyfile" 'CADDYSQLITE_E2E_UNSET'
neg "unknown alias fails" "$TMP/neg-alias.Caddyfile" 'unknown database'
neg "unsupported backup method fails" "$TMP/neg-method.Caddyfile" 'unsupported method'

# ── Summary ────────────────────────────────────────────
echo ""
echo "=========================================="
echo "  PASS: $PASS   FAIL: $FAIL"
echo "=========================================="
[ "$FAIL" -eq 0 ] && echo "ALL TESTS PASSED" || echo "SOME TESTS FAILED"
exit $FAIL