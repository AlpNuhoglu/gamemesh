package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	// The page changes with every build and names the hashed files, so a
	// browser must ask again each time.
	cachePage = "no-cache"
	// A file under assets/ is named after its content: it never changes.
	cacheAsset = "public, max-age=31536000, immutable"
)

// contentTypes is the table the responses use. The host's mime database is not
// consulted: on a minimal image it can be empty, and some give .js a type the
// browser refuses as a module script.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript",
	".mjs":   "text/javascript",
	".css":   "text/css",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".woff2": "font/woff2",
	".ico":   "image/x-icon",
	".map":   "application/json",
}

// Register mounts the client on r: /play (a redirect), /play/ (the page) and
// /play/assets/*filepath (its files), for GET and HEAD. Nothing else under
// /play exists, and there is no catch-all: the client has one page and does
// its own routing in the browser.
//
// app is the client's files, or nil when it was not built: the page then
// answers 503 and says how to build it.
func Register(r gin.IRoutes, app fs.FS) {
	methods := []string{http.MethodGet, http.MethodHead}

	r.Match(methods, "/play", guarded(func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, "/play/")
	}))
	r.Match(methods, "/play/", guarded(func(c *gin.Context) {
		if app == nil {
			c.String(http.StatusServiceUnavailable, "the browser client is not built into this gateway; run `make web` and rebuild\n")
			return
		}
		c.Header("Cache-Control", cachePage)
		serve(c, app, "index.html")
	}))
	r.Match(methods, "/play/assets/*filepath", guarded(func(c *gin.Context) {
		if app == nil {
			c.Status(http.StatusNotFound)
			return
		}
		// The wildcard keeps its leading slash. fs.ValidPath refuses "..", a
		// leading slash and empty elements, so a request cannot leave assets/
		// however it is encoded (the router decodes %2e and %2f before here).
		name := "assets/" + strings.TrimPrefix(c.Param("filepath"), "/")
		if !fs.ValidPath(name) {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", cacheAsset)
		serve(c, app, name)
	}))
}

// guarded runs h after the security headers are set, so that every response of
// these routes, errors and redirects included, carries them.
func guarded(h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		hd := c.Writer.Header()
		hd.Set("Content-Security-Policy", contentSecurityPolicy(c.Request.Host))
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		h(c)
	}
}

// contentSecurityPolicy is the policy for a page served from host. connect-src
// names the WebSocket origin as well as 'self': some browsers do not match a
// ws: URL against 'self'.
func contentSecurityPolicy(host string) string {
	connect := "'self'"
	if validHost(host) {
		connect += " ws://" + host + " wss://" + host
	}
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src " + connect + "; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"
}

// validHost reports whether host is only what a host[:port] can hold. The Host
// header is the client's, and it ends up in a header value.
func validHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == ':', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}

// serve answers with the file name from app, or 404 for anything that cannot
// be read as a file: a missing one, and a directory (reading one is an error,
// so it is never listed).
func serve(c *gin.Context, app fs.FS, name string) {
	data, err := fs.ReadFile(app, name)
	if err != nil {
		c.Header("Cache-Control", "no-store")
		c.Status(http.StatusNotFound)
		return
	}
	ct, ok := contentTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		ct = "application/octet-stream"
	}
	c.Data(http.StatusOK, ct, data)
}
