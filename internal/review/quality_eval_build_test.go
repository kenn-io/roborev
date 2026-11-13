package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

func qualitySupportedBuildSettings(settings []debug.BuildSetting) error {
	tags := ""
	for _, setting := range settings {
		switch setting.Key {
		case "-tags":
			tags = setting.Value
		case "-race", "-msan", "-asan":
			if setting.Value != "false" && setting.Value != "" {
				return fmt.Errorf("unsupported evaluation build setting %s", setting.Key)
			}
		case "-gcflags", "-asmflags", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "GOEXPERIMENT", "GOFIPS140", "-cover", "-coverpkg", "-covermode":
			if setting.Value != "" {
				return fmt.Errorf("unsupported evaluation build setting %s", setting.Key)
			}
		case "-compiler":
			if setting.Value != "gc" {
				return fmt.Errorf("unsupported evaluation compiler %q", setting.Value)
			}
		}
	}
	if tags != "qualityeval" {
		return fmt.Errorf("evaluation requires exactly compiler tags=qualityeval, got %q", tags)
	}
	return nil
}

func qualityCurrentBuildSettings() ([]debug.BuildSetting, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, fmt.Errorf("compiled Go build settings unavailable")
	}
	return info.Settings, nil
}

func qualitySelectionEnv(settings []debug.BuildSetting) []string {
	var env []string
	for _, setting := range settings {
		switch setting.Key {
		case "GOOS", "GOARCH", "CGO_ENABLED", "GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM":
			env = append(env, setting.Key+"="+setting.Value)
		}
	}
	return env
}

func qualitySelectionFingerprint(root, harness, overlayFile, modfile string) (string, error) {
	if _, err := qualityReadOverlay(root, harness, overlayFile); err != nil {
		return "", err
	}
	if modfile == "" {
		modfile = filepath.Join(root, "go.mod")
	}
	var content []byte
	content = append(content, []byte("qualityeval\x00"+runtime.Version()+"\x00")...)
	for _, name := range []string{modfile, strings.TrimSuffix(modfile, ".mod") + ".sum", overlayFile} {
		if name == "" {
			content = append(content, 0)
			continue
		}
		absolute, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(absolute)
		if os.IsNotExist(err) && strings.HasSuffix(name, ".sum") {
			data = []byte("absent")
		} else if err != nil {
			return "", err
		}
		content = append(content, []byte(absolute+"\x00")...)
		content = append(content, data...)
		content = append(content, 0)
	}
	return qualityHash(content), nil
}

func qualitySourceInventoryHash(inputs qualitySourceInputs) (string, error) {
	data, err := json.Marshal(struct {
		GoVersion string
		Modules   map[string]string
		Hashes    map[string]string
	}{GoVersion: inputs.GoVersion, Modules: inputs.Modules, Hashes: inputs.Hashes})
	if err != nil {
		return "", err
	}
	return qualityHash(data), nil
}

func qualityCheckCompiledSourceInventory(compiled string, inputs qualitySourceInputs) error {
	if compiled == "" {
		return fmt.Errorf("evaluation child is missing its compiled source inventory")
	}
	expected, err := qualitySourceInventoryHash(inputs)
	if err != nil {
		return err
	}
	if compiled != expected {
		return fmt.Errorf("compiled evaluation source inventory differs from runtime inputs")
	}
	return nil
}

func qualityWriteModuleCacheSnapshot(directory string, inputs qualitySourceInputs) (string, error) {
	if inputs.ModuleCacheRoot == "" {
		return "", fmt.Errorf("module cache root is missing from source inventory")
	}
	sourceRoot, err := filepath.Abs(inputs.ModuleCacheRoot)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(directory, "module-cache")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(inputs.Files))
	for key := range inputs.Files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := map[string]string{}
	for _, key := range keys {
		physical, err := filepath.Abs(inputs.Files[key])
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(filepath.VolumeName(sourceRoot), filepath.VolumeName(physical)) {
			continue
		}
		rel, err := filepath.Rel(sourceRoot, physical)
		if err != nil {
			return "", err
		}
		if !filepath.IsLocal(rel) {
			continue
		}
		rel, err = qualityRelativePath(sourceRoot, physical)
		if err != nil {
			return "", fmt.Errorf("selected module cache input escapes its source root: %q", key)
		}
		content, ok := inputs.SnapshotData[key]
		if !ok {
			return "", fmt.Errorf("module cache input bytes missing from source inventory: %q", key)
		}
		hash := qualityHash(content)
		if hash != inputs.Hashes[key] {
			return "", fmt.Errorf("module cache input bytes differ from source inventory: %q", key)
		}
		rel = filepath.Clean(rel)
		if previous, ok := seen[rel]; ok {
			if previous != hash {
				return "", fmt.Errorf("module cache snapshot path has conflicting inputs: %q", rel)
			}
			continue
		}
		copyPath := filepath.Join(destination, rel)
		if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(copyPath, content, 0o600); err != nil {
			return "", err
		}
		seen[rel] = hash
	}
	moduleKeys := make([]string, 0, len(inputs.ModuleCacheDirs))
	for key := range inputs.ModuleCacheDirs {
		moduleKeys = append(moduleKeys, key)
	}
	sort.Strings(moduleKeys)
	for _, key := range moduleKeys {
		moduleDir, err := filepath.Abs(inputs.ModuleCacheDirs[key])
		if err != nil {
			return "", err
		}
		relDir, err := filepath.Rel(sourceRoot, moduleDir)
		if err != nil || !filepath.IsLocal(relDir) {
			return "", fmt.Errorf("selected module directory escaped its cache root: %q", key)
		}
		relDir, err = qualityRelativePath(sourceRoot, moduleDir)
		if err != nil {
			return "", fmt.Errorf("selected module directory escaped its cache root: %q", key)
		}
		versionDir := filepath.Base(relDir)
		separator := strings.LastIndex(versionDir, "@")
		if separator <= 0 || separator == len(versionDir)-1 {
			return "", fmt.Errorf("selected module cache directory has no version: %q", key)
		}
		if err := os.MkdirAll(filepath.Join(destination, relDir), 0o700); err != nil {
			return "", err
		}
		modulePath := filepath.Join(filepath.Dir(relDir), versionDir[:separator])
		version := versionDir[separator+1:]
		sourceMetadata := filepath.Join(sourceRoot, "cache", "download", modulePath, "@v")
		destinationMetadata := filepath.Join(destination, "cache", "download", modulePath, "@v")
		for _, extension := range []string{".info", ".ziphash"} {
			name := version + extension
			data, err := os.ReadFile(filepath.Join(sourceMetadata, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(destinationMetadata, 0o700); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(destinationMetadata, name), data, 0o600); err != nil {
				return "", err
			}
		}
	}
	return destination, nil
}

func qualityWriteSourceSnapshotModfile(directory, modfile string, inputs qualitySourceInputs) (string, error) {
	modKey, sumKey := "go.mod", "go.sum"
	if modfile != "" {
		modKey, sumKey = "selected-go.mod", "selected-go.sum"
	}
	content, ok := inputs.SnapshotData[modKey]
	if !ok || qualityHash(content) != inputs.Hashes[modKey] {
		return "", fmt.Errorf("selected module file bytes missing or inconsistent with source inventory")
	}
	filename := filepath.Join(directory, "selected.mod")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		return "", err
	}
	if sum, ok := inputs.SnapshotData[sumKey]; ok {
		if qualityHash(sum) != inputs.Hashes[sumKey] {
			return "", fmt.Errorf("selected module sum bytes differ from source inventory")
		}
		sumfile := strings.TrimSuffix(filename, ".mod") + ".sum"
		if err := os.WriteFile(sumfile, sum, 0o600); err != nil {
			return "", err
		}
	}
	return filename, nil
}

func qualityWriteSourceSnapshotOverlay(root, harness, overlayFile, directory string, inputs qualitySourceInputs) (string, error) {
	replacements, err := qualityReadOverlay(root, harness, overlayFile)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(directory, "inputs"), 0o700); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(inputs.CompilerPaths))
	for key := range inputs.CompilerPaths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seenTargets := map[string]string{}
	for i, key := range keys {
		target := filepath.Clean(inputs.CompilerPaths[key])
		if !filepath.IsAbs(target) {
			return "", fmt.Errorf("compiler input path is not absolute: %q", target)
		}
		content, ok := inputs.SnapshotData[key]
		if !ok {
			return "", fmt.Errorf("compiler input bytes missing from source inventory: %q", key)
		}
		if qualityHash(content) != inputs.Hashes[key] {
			return "", fmt.Errorf("compiler input bytes differ from source inventory: %q", key)
		}
		if previous, ok := seenTargets[target]; ok {
			if previous != inputs.Hashes[key] {
				return "", fmt.Errorf("compiler input target has conflicting inventory bytes: %q", target)
			}
			continue
		}
		copyPath := filepath.Join(directory, "inputs", fmt.Sprintf("%04d", i), filepath.Base(target))
		if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(copyPath, content, 0o400); err != nil {
			return "", err
		}
		seenTargets[target] = inputs.Hashes[key]
		replacements[target] = copyPath
	}
	overlayData, err := json.Marshal(struct {
		Replace map[string]string
	}{Replace: replacements})
	if err != nil {
		return "", err
	}
	filename := filepath.Join(directory, "source-overlay.json")
	if err := os.WriteFile(filename, overlayData, 0o600); err != nil {
		return "", err
	}
	return filename, nil
}
