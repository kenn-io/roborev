package gitfixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Metadata identifies the real Git and fixture config for a canonical executable.
type Metadata struct {
	RealGit     string
	Global      string
	Home        string
	UserProfile string
}

// MetadataSuffix identifies a fixture launcher sidecar.
const MetadataSuffix = ".roborev-git-fixture.json"

// ReadMetadata reads the sidecar belonging to the canonical executable.
func ReadMetadata(executable string) (Metadata, error) {
	var metadata Metadata
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return metadata, err
	}
	data, err := os.ReadFile(executable + MetadataSuffix)
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, err
	}
	if !filepath.IsAbs(metadata.RealGit) || !filepath.IsAbs(metadata.Global) {
		return metadata, fmt.Errorf("fixture Git metadata requires absolute executable and config paths")
	}
	return metadata, nil
}
