package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/stretchr/testify/require"
)

// ArchiveEntry describes one file or link in a test archive.
type ArchiveEntry struct {
	Name     string
	Content  string
	TypeFlag byte
	LinkName string
	Mode     int64
}

// CreateTarGzip creates a compressed tar fixture with the supplied entries.
func CreateTarGzip(t *testing.T, entries []ArchiveEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		typeFlag := entry.TypeFlag
		if typeFlag == 0 {
			typeFlag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.Name,
			Mode:     mode,
			Size:     int64(len(entry.Content)),
			Typeflag: typeFlag,
			Linkname: entry.LinkName,
		}
		require.NoError(t, tw.WriteHeader(header))
		if len(entry.Content) > 0 {
			_, err := tw.Write([]byte(entry.Content))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}
