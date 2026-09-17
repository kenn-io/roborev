//go:build cgo && !windows

package searchindex

// sqlitevec selects its C extension when CGO is enabled. The blank import
// supplies the SQLite symbols that extension needs at link time.
import _ "github.com/mattn/go-sqlite3"
