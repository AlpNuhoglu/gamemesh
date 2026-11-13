package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prodOrigins is ALLOWED_ORIGINS in deployments/k8s/base/config.env.
var prodOrigins = []string{"https://gamemesh.app", "https://*.gamemesh.app", "http://localhost:*", "http://127.0.0.1:*"}

func mustOrigins(t *testing.T, entries ...string) *OriginMatcher {
	t.Helper()
	m, err := ParseOrigins(entries)
	require.NoError(t, err)
	return m
}

func TestOriginMatcher(t *testing.T) {
	prod := mustOrigins(t, prodOrigins...)
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		// Exact.
		{"https://gamemesh.app", true},
		{"HTTPS://GameMesh.App", true},       // scheme and host are case-insensitive
		{"http://gamemesh.app", false},       // other scheme
		{"https://gamemesh.app:8443", false}, // other port
		{"https://gamemesh.app:443", false},  // a browser never sends the default port
		// Subdomain wildcard, any depth, same scheme.
		{"https://play.gamemesh.app", true},
		{"https://a.b.gamemesh.app", true},
		{"https://Play.GAMEMESH.app", true},
		{"http://play.gamemesh.app", false},
		{"https://evilgamemesh.app", false},
		{"https://gamemesh.app.evil.example", false},
		{"https://play.gamemesh.app.evil.example", false},
		{"https://.gamemesh.app", false},
		{"https://play.gamemesh.app:8443", false},
		// Port wildcard, with or without a port.
		{"http://localhost", true},
		{"http://localhost:5173", true},
		{"http://LOCALHOST:3000", true},
		{"http://127.0.0.1:8080", true},
		{"https://localhost:5173", false}, // other scheme
		{"http://localhost.evil.example:5173", false},
		{"http://localhost:0", false},
		{"http://localhost:99999", false},
		{"http://localhost:", false},
		// Not an origin a browser sends.
		{"null", false},
		{"", false},
		{"https://gamemesh.app/", false},             // trailing slash
		{"https://gamemesh.app/play", false},         // path
		{"https://gamemesh.app?x=1", false},          // query
		{"https://user@gamemesh.app", false},         // user info
		{"https://evil.example@gamemesh.app", false}, // user info, the other way round
		{"gamemesh.app", false},                      // no scheme
		{"wss://gamemesh.app", false},
	} {
		assert.Equal(t, tc.want, prod.Allowed(tc.origin), "%q", tc.origin)
	}
}

func TestOriginMatcherAllowAll(t *testing.T) {
	m := mustOrigins(t, "*")
	assert.True(t, m.AllowsAny())
	for _, o := range []string{"https://anything.example", "null", "http://localhost:1"} {
		assert.True(t, m.Allowed(o), o)
	}
	assert.False(t, mustOrigins(t, prodOrigins...).AllowsAny())
}

func TestOriginMatcherEmptyAndNil(t *testing.T) {
	assert.False(t, mustOrigins(t).Allowed("https://gamemesh.app"), "an empty list allows nothing")
	var m *OriginMatcher
	assert.False(t, m.Allowed("https://gamemesh.app"), "a nil matcher allows nothing")
	assert.False(t, m.AllowsAny())
}

func TestOriginMatcherIPv6(t *testing.T) {
	m := mustOrigins(t, "http://[::1]:*")
	assert.True(t, m.Allowed("http://[::1]:5173"))
	assert.True(t, m.Allowed("http://[::1]"))
	assert.False(t, m.Allowed("http://[::2]:5173"))
}

// TestParseOriginsRejectsBadEntries: an entry that does not parse is an
// error (the service refuses to start), never a rule that matches nothing.
func TestParseOriginsRejectsBadEntries(t *testing.T) {
	for _, bad := range []string{
		"gamemesh.app",              // no scheme
		"https://",                  // no host
		"https://gamemesh.app/",     // trailing slash
		"https://gamemesh.app/play", // path
		"ftp://gamemesh.app",        // not http(s)
		"https://*",                 // wildcard without a domain
		"https://a*.gamemesh.app",   // wildcard not a whole label
		"https://play.*.gamemesh.app",
		"https://*.*.gamemesh.app",
		"https://gamemesh.app:http",
		"https://gamemesh.app:70000",
		"https://gamemesh.app:",
		"https://game mesh.app",
		"null",
	} {
		_, err := ParseOrigins([]string{bad})
		assert.Error(t, err, "%q", bad)
	}
	_, err := ParseOrigins([]string{"*", "https://gamemesh.app"})
	assert.Error(t, err, `"*" mixed with other entries`)

	_, err = ParseOrigins(prodOrigins)
	assert.NoError(t, err)
}
