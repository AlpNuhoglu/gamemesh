package ghosttag

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

// The game core reaches the service only through the interfaces of
// ports.go. These checks on the source keep it that way: if one fails, a
// change has made the core depend on a transport, a store client, the
// event bus or the WebSocket service. Do not relax the test; move the code
// to the adapter.

// forbidden are the imports the core must never have, by path prefix.
var forbidden = []string{
	"github.com/gorilla/",
	"github.com/redis/",
	"github.com/nats-io/",
	"github.com/alpnuhoglu/gamemesh/pkg/events",
	"github.com/alpnuhoglu/gamemesh/internal/outbox",
	"github.com/alpnuhoglu/gamemesh/internal/wsgateway",
}

// coreImports returns every import of every non-test file of the package,
// by file.
func coreImports(t *testing.T) map[string][]string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := make(map[string][]string)
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		out[name] = importPaths(t, f)
	}
	// The parse found the package: without this the checks would pass on
	// an empty set if the files moved.
	require.Contains(t, out, "ports.go")
	require.Contains(t, out, "session.go")
	require.Contains(t, out, "room.go")
	return out
}

func importPaths(t *testing.T, f *ast.File) []string {
	t.Helper()
	var paths []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		paths = append(paths, p)
	}
	return paths
}

// TestCoreImportsNoTransportStoreOrBus: none of the forbidden packages.
func TestCoreImportsNoTransportStoreOrBus(t *testing.T) {
	for name, paths := range coreImports(t) {
		for _, p := range paths {
			for _, bad := range forbidden {
				assert.False(t, strings.HasPrefix(p, bad), "%s imports %s", name, p)
			}
		}
	}
}

// TestCoreUsesTheStandardLibraryOnly: every import of non-test code is a
// standard library package (its first path element has no dot).
func TestCoreUsesTheStandardLibraryOnly(t *testing.T) {
	for name, paths := range coreImports(t) {
		for _, p := range paths {
			first, _, _ := strings.Cut(p, "/")
			assert.NotContains(t, first, ".", "%s imports %s, which is not the standard library", name, p)
		}
	}
}
