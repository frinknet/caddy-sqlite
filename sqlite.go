package caddysqlite

import (
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/frinknet/caddy-sqlite/pkg/database"
	"github.com/frinknet/caddy-sqlite/pkg/plugin"
)

func init() {
	if err := database.RegisterHTTPFuncs(); err != nil {
		panic(err)
	}

	caddy.RegisterModule(plugin.SQLiteDBs{})
	caddy.RegisterModule(plugin.SQLiteResponse{})
	caddy.RegisterModule(plugin.SQLiteTemplateFuncs{})
	caddy.RegisterModule(new(plugin.SQLiteBG))

	httpcaddyfile.RegisterGlobalOption("sqlite_db", plugin.ParseSQLiteDBOption)
	httpcaddyfile.RegisterHandlerDirective("sqlite_response", plugin.ParseResponse)
	httpcaddyfile.RegisterHandlerDirective("sqlite_bg", plugin.ParseBG)

	httpcaddyfile.RegisterDirectiveOrder("sqlite_response", "before", "respond")
	httpcaddyfile.RegisterDirectiveOrder("sqlite_bg", "before", "respond")
}
