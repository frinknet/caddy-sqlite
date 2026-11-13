package caddysqlite

import (
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/frinknet/caddy-sqlite/pkg/plugin"
)

func init() {
	caddy.RegisterModule(plugin.SQLiteDBs{})
	caddy.RegisterModule(plugin.SQLiteResponse{})
	caddy.RegisterModule(plugin.SQLiteTemplateFuncs{})

	httpcaddyfile.RegisterGlobalOption("sqlite_db", plugin.ParseSQLiteDBOption)
	httpcaddyfile.RegisterHandlerDirective("sqlite_response", plugin.ParseResponse)

	httpcaddyfile.RegisterDirectiveOrder("sqlite_response", "before", "respond")
}
