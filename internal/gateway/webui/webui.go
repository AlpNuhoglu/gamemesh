// Package webui serves the browser client the gateway embeds, under /play/.
//
// The client is built by Vite into embedded/app (npm run build in web/, or the
// Node stage of the gateway's image). That output is not committed; only the
// README next to it is, so this package compiles on a checkout that has not
// built the client. App reports whether there is a client to serve.
package webui

import (
	"embed"
	"io/fs"
)

// The all: prefix is not needed for the build output (Vite writes no dotfiles);
// it keeps the embed from silently dropping one if that changes.
//
//go:embed all:embedded
var embedded embed.FS

// App returns the built client's files, rooted at embedded/app, and whether
// index.html is in it. A false means the client was not built.
func App() (fs.FS, bool) {
	sub, err := fs.Sub(embedded, "embedded/app")
	if err != nil {
		return nil, false
	}
	return appIfBuilt(sub)
}

// appIfBuilt is App's decision on any filesystem: the client is there only if
// its page is.
func appIfBuilt(app fs.FS) (fs.FS, bool) {
	info, err := fs.Stat(app, "index.html")
	if err != nil || info.IsDir() {
		return nil, false
	}
	return app, true
}
