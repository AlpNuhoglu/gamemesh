package webui

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pageBody = "<!doctype html><title>page</title>"
	jsBody   = "console.log(1)"
)

func testApp() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte(pageBody)},
		"assets/index-abc123.js":      {Data: []byte(jsBody)},
		"assets/index-abc123.css":     {Data: []byte("a{}")},
		"assets/data.json":            {Data: []byte("{}")},
		"assets/logo.svg":             {Data: []byte("<svg/>")},
		"assets/logo.png":             {Data: []byte("png")},
		"assets/font.woff2":           {Data: []byte("woff2")},
		"assets/favicon.ico":          {Data: []byte("ico")},
		"assets/blob.unknown":         {Data: []byte("x")},
		"assets/sub/nested.js":        {Data: []byte("nested")},
		"secret.txt":                  {Data: []byte("outside assets")},
		"assets-other/not-assets.txt": {Data: []byte("sibling")},
	}
}

// lenientFS is a filesystem that does not refuse an invalid path but cleans
// it, as a directory on disk would. embed.FS and MapFS both refuse "..", so
// with them the handler's own check cannot be seen working; with this one it
// is the only thing between a request and the file outside assets/.
type lenientFS struct{ fstest.MapFS }

func (l lenientFS) Open(name string) (fs.File, error) {
	return l.MapFS.Open(strings.TrimPrefix(path.Clean("/"+name), "/"))
}

func (l lenientFS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(l.MapFS, strings.TrimPrefix(path.Clean("/"+name), "/"))
}

func (l lenientFS) ReadFile(name string) ([]byte, error) {
	return l.MapFS.ReadFile(strings.TrimPrefix(path.Clean("/"+name), "/"))
}

func server(t *testing.T, built bool) *httptest.Server {
	t.Helper()
	return serverWith(t, built, testApp())
}

func serverWith(t *testing.T, built bool, app fs.FS) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if built {
		Register(r, app)
	} else {
		Register(r, nil)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// do sends a request without following redirects and returns the response and
// its body.
func do(t *testing.T, method, url string) (*http.Response, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

func requireSecurityHeaders(t *testing.T, resp *http.Response, host string) {
	t.Helper()
	assert.Equal(t, "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
		"connect-src 'self' ws://"+host+" wss://"+host+"; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
		resp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
	assert.Equal(t, "same-origin", resp.Header.Get("Cross-Origin-Opener-Policy"))
}

func TestPlayRedirectsToTrailingSlash(t *testing.T) {
	srv := server(t, true)
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		resp, _ := do(t, m, srv.URL+"/play")
		assert.Equal(t, http.StatusPermanentRedirect, resp.StatusCode, m)
		assert.Equal(t, "/play/", resp.Header.Get("Location"), m)
		requireSecurityHeaders(t, resp, srv.Listener.Addr().String())
	}
}

func TestPageIsServedWithoutCaching(t *testing.T) {
	srv := server(t, true)
	resp, body := do(t, http.MethodGet, srv.URL+"/play/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, pageBody, body)
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	requireSecurityHeaders(t, resp, srv.Listener.Addr().String())
}

func TestCSPNamesTheRequestHost(t *testing.T) {
	srv := server(t, true)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/play/", nil)
	require.NoError(t, err)
	req.Host = "api.example.test"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	requireSecurityHeaders(t, resp, "api.example.test")
}

func TestHeadAnswersLikeGetWithoutABody(t *testing.T) {
	srv := server(t, true)
	for _, p := range []string{"/play/", "/play/assets/index-abc123.js"} {
		get, _ := do(t, http.MethodGet, srv.URL+p)
		head, body := do(t, http.MethodHead, srv.URL+p)
		assert.Equal(t, http.StatusOK, head.StatusCode, p)
		assert.Empty(t, body, p)
		assert.Equal(t, get.Header.Get("Cache-Control"), head.Header.Get("Cache-Control"), p)
		assert.Equal(t, get.Header.Get("Content-Type"), head.Header.Get("Content-Type"), p)
		assert.Equal(t, get.Header.Get("Content-Length"), head.Header.Get("Content-Length"), p)
		requireSecurityHeaders(t, head, srv.Listener.Addr().String())
	}
}

func TestAssetsAreImmutableAndTyped(t *testing.T) {
	srv := server(t, true)
	cases := []struct{ path, contentType, body string }{
		{"/play/assets/index-abc123.js", "text/javascript", jsBody},
		{"/play/assets/index-abc123.css", "text/css", "a{}"},
		{"/play/assets/data.json", "application/json", "{}"},
		{"/play/assets/logo.svg", "image/svg+xml", "<svg/>"},
		{"/play/assets/logo.png", "image/png", "png"},
		{"/play/assets/font.woff2", "font/woff2", "woff2"},
		{"/play/assets/favicon.ico", "image/x-icon", "ico"},
		{"/play/assets/blob.unknown", "application/octet-stream", "x"},
		{"/play/assets/sub/nested.js", "text/javascript", "nested"},
	}
	for _, tc := range cases {
		resp, body := do(t, http.MethodGet, srv.URL+tc.path)
		assert.Equal(t, http.StatusOK, resp.StatusCode, tc.path)
		assert.Equal(t, tc.body, body, tc.path)
		assert.Equal(t, tc.contentType, resp.Header.Get("Content-Type"), tc.path)
		assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"), tc.path)
		requireSecurityHeaders(t, resp, srv.Listener.Addr().String())
	}
}

func TestMissingAssetIsNotFoundAndNotCached(t *testing.T) {
	srv := server(t, true)
	resp, _ := do(t, http.MethodGet, srv.URL+"/play/assets/missing.js")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.NotContains(t, resp.Header.Get("Cache-Control"), "immutable", "a 404 must not be cached for a year")
	requireSecurityHeaders(t, resp, srv.Listener.Addr().String())
}

func TestAssetsCannotLeaveTheirDirectory(t *testing.T) {
	t.Run("filesystem that refuses invalid paths", func(t *testing.T) { assetsCannotLeave(t, server(t, true)) })
	t.Run("filesystem that cleans them", func(t *testing.T) { assetsCannotLeave(t, serverWith(t, true, lenientFS{testApp()})) })
}

func assetsCannotLeave(t *testing.T, srv *httptest.Server) {
	t.Helper()
	paths := []string{
		"/play/assets/..%2findex.html",
		"/play/assets/%2e%2e/index.html",
		"/play/assets/%2e%2e%2findex.html",
		"/play/assets/../index.html",
		"/play/assets/..%2fsecret.txt",
		"/play/assets/%2e%2e%2f%2e%2e%2fsecret.txt",
		"/play/assets//index.html",
		"/play/assets/sub/../../index.html",
		"/play/assets/..%5cindex.html",
	}
	for _, p := range paths {
		resp, body := do(t, http.MethodGet, srv.URL+p)
		assert.NotEqual(t, http.StatusOK, resp.StatusCode, p)
		assert.NotContains(t, body, pageBody, p)
		assert.NotContains(t, body, "outside assets", p)
	}
	// The ones that reach the handler are refused outright, not redirected.
	resp, _ := do(t, http.MethodGet, srv.URL+"/play/assets/..%2findex.html")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDirectoriesAreNeverListed(t *testing.T) {
	srv := server(t, true)
	for _, p := range []string{"/play/assets/", "/play/assets/sub", "/play/assets/sub/"} {
		resp, body := do(t, http.MethodGet, srv.URL+p)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		assert.NotContains(t, body, "nested.js", p)
	}
}

func TestNoRouteOutsideTheThree(t *testing.T) {
	srv := server(t, true)
	for _, p := range []string{"/play/index.html", "/play/other", "/playx", "/play/secret.txt", "/"} {
		resp, _ := do(t, http.MethodGet, srv.URL+p)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
	}
	// gin redirects a path that lacks the slash of a route it has; the target
	// is the assets directory, which answers 404 (TestDirectoriesAreNeverListed).
	resp, _ := do(t, http.MethodGet, srv.URL+"/play/assets")
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r, _ := do(t, m, srv.URL+"/play/")
		assert.Equal(t, http.StatusNotFound, r.StatusCode, m)
	}
}

func TestUnbuiltClient(t *testing.T) {
	srv := server(t, false)

	resp, body := do(t, http.MethodGet, srv.URL+"/play/")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, body, "make web")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	requireSecurityHeaders(t, resp, srv.Listener.Addr().String())

	resp, _ = do(t, http.MethodGet, srv.URL+"/play/assets/index-abc123.js")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, _ = do(t, http.MethodGet, srv.URL+"/play")
	assert.Equal(t, http.StatusPermanentRedirect, resp.StatusCode)
}

func TestAppIfBuilt(t *testing.T) {
	app, ok := appIfBuilt(testApp())
	assert.True(t, ok)
	assert.NotNil(t, app)

	app, ok = appIfBuilt(fstest.MapFS{"assets/x.js": {Data: []byte("x")}})
	assert.False(t, ok)
	assert.Nil(t, app)

	_, ok = appIfBuilt(fstest.MapFS{"index.html/x": {Data: []byte("x")}})
	assert.False(t, ok, "a directory named index.html is not the page")
}
