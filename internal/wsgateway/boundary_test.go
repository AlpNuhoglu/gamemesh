package wsgateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The boundary between ephemeral and durable traffic, as checks on the source.
//
// Ephemeral: the relay (relay.go), client -> hub -> clients, in memory, lossy.
// Durable:   backend events, service -> JetStream/outbox -> bridge.go -> hub.
//
// The two meet only inside the hub, and only in one direction: the bus
// delivers into the hub, and nothing a client sends ever reaches the bus. If
// one of these tests fails, a change has moved game traffic onto the durable
// path or opened a way for it to get there. Do not relax the test; move the
// code. See docs/latency.md, "Message classes".

const (
	eventsPkg = "github.com/alpnuhoglu/gamemesh/pkg/events"
	outboxPkg = "github.com/alpnuhoglu/gamemesh/internal/outbox"
)

// busClients are the transports the durable path runs on. The relay must not
// reach any of them, with or without pkg/events in between.
var busClients = []string{"github.com/nats-io/", "github.com/redis/"}

// packageFiles parses every non-test Go file of this package.
func packageFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files[name] = f
	}
	require.Contains(t, files, "relay.go")
	require.Contains(t, files, "bridge.go")
	return files
}

func imports(t *testing.T, f *ast.File) []string {
	t.Helper()
	var paths []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		paths = append(paths, p)
	}
	return paths
}

// The relay code has no way to name the bus, the outbox or a transport.
func TestRelayDoesNotImportTheDurablePath(t *testing.T) {
	for _, p := range imports(t, packageFiles(t)["relay.go"]) {
		assert.NotEqual(t, eventsPkg, p, "relay.go must not import the event bus")
		assert.NotEqual(t, outboxPkg, p, "relay.go must not import the outbox")
		for _, client := range busClients {
			assert.False(t, strings.HasPrefix(p, client), "relay.go must not import %s", p)
		}
	}
}

// Only the bridge, which consumes backend events, may know the event bus;
// nothing in the WebSocket service writes to the outbox or talks to a
// transport directly.
func TestOnlyTheBridgeKnowsTheEventBus(t *testing.T) {
	for name, f := range packageFiles(t) {
		for _, p := range imports(t, f) {
			if p == eventsPkg {
				assert.Equal(t, "bridge.go", name, "%s imports the event bus; only bridge.go may", name)
			}
			assert.NotEqual(t, outboxPkg, p, "%s imports the outbox", name)
			for _, client := range busClients {
				assert.False(t, strings.HasPrefix(p, client), "%s imports %s", name, p)
			}
		}
	}
}

// The WebSocket service only consumes from the bus. With no Publish call in
// the package, nothing a client sends can be put on JetStream, whatever it is
// called.
func TestWebSocketServiceNeverPublishes(t *testing.T) {
	for name, f := range packageFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				assert.NotEqual(t, "Publish", sel.Sel.Name, "%s calls Publish", name)
			}
			return true
		})
	}
}

// The relay's frame type is not the name of any durable event, so the two
// classes cannot be confused on the wire, by a client or by a later change.
func TestRelayTypeIsNotADurableEventType(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "pkg", "events", "events.go"), nil, 0)
	require.NoError(t, err)

	durable := make(map[string]string) // value -> constant name
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "Type") || i >= len(spec.Values) {
				continue
			}
			if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				v, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				durable[v] = name.Name
			}
		}
		return true
	})
	// The parse found the constants: without this the check would pass on
	// an empty set if events.go were moved or its constants renamed.
	require.Contains(t, durable, "MatchFound")
	require.Contains(t, durable, "LeaderboardUpdated")

	assert.NotContains(t, durable, relayType,
		"%q is both the relay's frame type and a durable event type", relayType)
}
