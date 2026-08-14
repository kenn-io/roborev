package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

const (
	viteManifestPath             = ".vite/manifest.json"
	productionDistributionMarker = `<meta name="roborev-web-distribution" content="production"`
	compilationStub              = "<!doctype html>\n<html lang=\"en\">\n  <head><meta charset=\"utf-8\"><title>Roborev</title></head>\n  <body>Roborev web assets are not built.</body>\n</html>\n"
)

type viteManifestEntry struct {
	File   string   `json:"file"`
	CSS    []string `json:"css"`
	Assets []string `json:"assets"`
}

type assetCatalog struct {
	immutable map[string]struct{}
	stub      bool
}

func loadDistribution(files fs.FS) (*assetCatalog, error) {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read web index: %w", err)
	}
	if isCompilationStub(index) {
		return &assetCatalog{immutable: make(map[string]struct{}), stub: true}, nil
	}
	return validateReleaseDistribution(files)
}

func validateReleaseDistribution(files fs.FS) (*assetCatalog, error) {
	if err := validateDistributionFiles(files); err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read web index: %w", err)
	}
	if isCompilationStub(index) {
		return nil, fmt.Errorf("embedded web distribution is the compilation stub")
	}
	if !strings.Contains(string(index), productionDistributionMarker) {
		return nil, fmt.Errorf("web index is missing the production marker")
	}

	manifestBytes, err := fs.ReadFile(files, viteManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read Vite manifest: %w", err)
	}
	manifest := make(map[string]viteManifestEntry)
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode Vite manifest: %w", err)
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("vite manifest is empty")
	}

	catalog := &assetCatalog{immutable: make(map[string]struct{})}
	for _, entry := range manifest {
		paths := append([]string{entry.File}, entry.CSS...)
		paths = append(paths, entry.Assets...)
		for _, assetPath := range paths {
			if err := validateAssetPath(assetPath); err != nil {
				return nil, err
			}
			regular, err := isRegularFile(files, assetPath)
			if err != nil {
				return nil, fmt.Errorf("manifest asset %q: %w", assetPath, err)
			}
			if !regular {
				return nil, fmt.Errorf("manifest asset %q is not a regular file", assetPath)
			}
			catalog.immutable[assetPath] = struct{}{}
		}
	}
	return catalog, nil
}

func validateDistributionFiles(files fs.FS) error {
	return fs.WalkDir(files, ".", func(assetPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if assetPath == "." || entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("distribution asset %q is not a regular file", assetPath)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect distribution asset %q: %w", assetPath, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("distribution asset %q is not a regular file", assetPath)
		}
		if assetPath == "index.html" || assetPath == viteManifestPath {
			return nil
		}
		return validateAssetPath(assetPath)
	})
}

func isCompilationStub(index []byte) bool {
	return strings.ReplaceAll(string(index), "\r\n", "\n") == compilationStub
}

func isRegularFile(files fs.FS, assetPath string) (bool, error) {
	entries, err := fs.ReadDir(files, path.Dir(assetPath))
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != path.Base(assetPath) {
			continue
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return false, nil
		}
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		return info.Mode().IsRegular(), nil
	}
	return false, fs.ErrNotExist
}

func validateAssetPath(assetPath string) error {
	if assetPath == "" || !fs.ValidPath(assetPath) || path.Clean(assetPath) != assetPath || strings.Contains(assetPath, `\`) {
		return fmt.Errorf("invalid manifest asset path %q", assetPath)
	}
	for segment := range strings.SplitSeq(assetPath, "/") {
		if strings.HasPrefix(segment, ".") {
			return fmt.Errorf("invalid manifest asset path %q", assetPath)
		}
	}
	base := strings.ToLower(path.Base(assetPath))
	extension := strings.ToLower(path.Ext(base))
	stem := strings.TrimSuffix(base, extension)
	secretNames := map[string]struct{}{
		"credential": {}, "credentials": {}, "secret": {}, "secrets": {},
		"token": {}, "tokens": {}, "password": {}, "passwd": {},
		"id_rsa": {}, "id_ed25519": {},
	}
	secretExtensions := map[string]struct{}{".key": {}, ".pem": {}, ".p12": {}, ".pfx": {}}
	if _, found := secretNames[stem]; found {
		return fmt.Errorf("secret-like manifest asset path %q", assetPath)
	}
	if _, found := secretExtensions[extension]; found {
		return fmt.Errorf("secret-like manifest asset path %q", assetPath)
	}
	return nil
}
