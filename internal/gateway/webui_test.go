package gateway

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
)

const clientPage = "<!doctype html><title>Ghost Tag</title>"

func clientFS() fs.FS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(clientPage)},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
	}
}

// getClient runs the gateway with the release flag and client files as given
// and returns the response to a GET (redirects are not followed).
func getClient(t *testing.T, enabled bool, app fs.FS, path string) (*http.Response, string) {
	t.Helper()
	cfg := config.Load("gateway")
	cfg.GameClientEnabled = enabled
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterRoutes(r, cfg, auth.NewTokenManager("gw-test-secret", time.Hour, "gamemesh"), nil, app, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

var clientPaths = []string{"/play", "/play/", "/play/assets/index-abc123.js"}

func TestClientIsNotThereWithTheFlagOff(t *testing.T) {
	for _, p := range clientPaths {
		// Even with files to serve: the flag, not the files, decides.
		resp, body := getClient(t, false, clientFS(), p)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		assert.NotContains(t, body, "Ghost Tag", p)
		assert.Empty(t, resp.Header.Get("Content-Security-Policy"), p)
	}
}

func TestClientIsServedWithTheFlagOn(t *testing.T) {
	resp, body := getClient(t, true, clientFS(), "/play/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, clientPage, body)
	assert.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))

	resp, body = getClient(t, true, clientFS(), "/play/assets/index-abc123.js")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "console.log(1)", body)

	resp, _ = getClient(t, true, clientFS(), "/play")
	assert.Equal(t, http.StatusPermanentRedirect, resp.StatusCode)
}

func TestClientNotBuiltAnswers503WithTheFlagOn(t *testing.T) {
	resp, body := getClient(t, true, nil, "/play/")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, body, "make web")

	resp, _ = getClient(t, true, nil, "/play/assets/index-abc123.js")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
