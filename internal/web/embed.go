package web

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed all:dist
var embeddedDistribution embed.FS

func embeddedFiles() (fs.FS, error) {
	files, err := fs.Sub(embeddedDistribution, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded web distribution: %w", err)
	}
	return files, nil
}

func NewEmbeddedHandler() (httpHandler, error) {
	files, err := embeddedFiles()
	if err != nil {
		return nil, err
	}
	return NewHandler(files)
}

func ValidateEmbeddedRelease() error {
	files, err := embeddedFiles()
	if err != nil {
		return err
	}
	_, err = validateReleaseDistribution(files)
	return err
}

// EmbeddedReleaseAvailable reports whether this binary contains a validated
// production distribution rather than the source-build compilation stub.
func EmbeddedReleaseAvailable() bool {
	return ValidateEmbeddedRelease() == nil
}
