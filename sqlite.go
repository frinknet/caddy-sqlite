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

// Registered as modules: the handler is a *SQLite because its CaddyModule has a
// pointer receiver (the type holds a sync.Mutex), while the app and the template
// functions are safe to register by value.
var (
	_ caddy.Module = plugin.Engine{}
	_ caddy.Module = (*plugin.SQLite)(nil)
	_ caddy.Module = plugin.SQLiteTemplateFuncs{}
)
