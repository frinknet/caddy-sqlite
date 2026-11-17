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

	caddy.RegisterModule(plugin.Engine{})
	caddy.RegisterModule(new(plugin.SQLite))
	caddy.RegisterModule(plugin.SQLiteTemplateFuncs{})

	httpcaddyfile.RegisterGlobalOption("sqlite", plugin.ParseGlobalOption)
	httpcaddyfile.RegisterHandlerDirective("sqlite", plugin.ParseHandler)

	httpcaddyfile.RegisterDirectiveOrder("sqlite", "before", "respond")
}
