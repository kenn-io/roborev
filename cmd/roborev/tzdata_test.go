package main

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTzdataEmbedded guards the blank time/tzdata import at the source
// level. A behavioral test is not possible: Go's timezone lookup always
// falls back to the platform zoneinfo directories, so on any dev or CI
// host time.LoadLocation succeeds whether or not the embed is present.
// The embed only matters on systems without a zoneinfo database (Windows
// releases, minimal containers), where dropping it would silently break
// named timezones such as the [ci.quiet_hours] timezone.
func TestTzdataEmbedded(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range file.Imports {
			if imp.Path.Value == `"time/tzdata"` {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	require.True(t, found,
		"package main must blank-import time/tzdata so named timezones work in release binaries without a system zoneinfo database")
}
