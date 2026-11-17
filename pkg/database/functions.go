package database

import (
	"database/sql/driver"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const (
	httpUDFTimeout = 5 * time.Second
	httpUDFMaxBody = 1 << 20 // 1 MiB
	httpUDFJSON    = "application/json"
)

// httpUDFClient is shared across every UDF call. One Transport means
// keep-alive connections are reused across webhook targets; Timeout is the
// only thing bounding how long a single hung endpoint can stall the SQLite
// writer, because the UDF callback runs synchronously on the connection's
// goroutine — inside commitBatch, if it's a trigger.
var httpUDFClient = &http.Client{
	Timeout: httpUDFTimeout,
	Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// RegisterHTTPFuncs installs http_get and http_post in the modernc.org/sqlite
// driver. Must be called before any DB is opened (i.e. from package init, not
// from Provision) — the driver resolves UDFs at sqlite3_open time, so a
// connection opened before this call would silently lack the functions.
//
//	http_get(url)              → GET,  body as TEXT, NULL on transport failure
//	http_post(url, body[, ct]) → POST, body as TEXT, NULL on transport failure
//	                             Content-Type defaults to application/json
//
// Bodies are capped at 1 MiB. Transport failures yield NULL rather than a SQL
// error, so a trigger firing a webhook doesn't roll back the surrounding write
// if the receiver is down. Arity/type errors return a SQL error — those are
// query bugs, not runtime conditions.
//
// HTTP responses are returned verbatim regardless of status code. No URL
// allowlist is applied: any bound parameter reaching these functions is
// SSRF-capable, so treat user-controlled binds as untrusted hosts.
func RegisterHTTPFuncs() error {
	if err := sqlite.RegisterScalarFunction("http_get", 1, httpGetFunc); err != nil {
		return fmt.Errorf("register http_get: %w", err)
	}
	if err := sqlite.RegisterScalarFunction("http_post", -1, httpPostFunc); err != nil {
		return fmt.Errorf("register http_post: %w", err)
	}
	return nil
}

func httpGetFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("http_get: expected 1 argument, got %d", len(args))
	}
	url, ok := args[0].(string)
	if !ok || url == "" {
		return nil, fmt.Errorf("http_get: url must be non-empty TEXT")
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil
	}
	resp, err := httpUDFClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, httpUDFMaxBody))
	if err != nil {
		return nil, nil
	}
	return string(body), nil
}

func httpPostFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, fmt.Errorf("http_post: expected 2 or 3 arguments, got %d", len(args))
	}
	url, ok := args[0].(string)
	if !ok || url == "" {
		return nil, fmt.Errorf("http_post: url must be non-empty TEXT")
	}
	body, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("http_post: body must be TEXT")
	}
	contentType := httpUDFJSON
	if len(args) == 3 {
		ct, ok := args[2].(string)
		if !ok || ct == "" {
			return nil, fmt.Errorf("http_post: content_type must be non-empty TEXT")
		}
		contentType = ct
	}

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, nil
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := httpUDFClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, httpUDFMaxBody))
	if err != nil {
		return nil, nil
	}
	return string(out), nil
}
