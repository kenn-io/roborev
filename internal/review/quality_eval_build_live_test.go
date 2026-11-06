//go:build qualityeval

package review

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Set only by the controlled evaluation-child build below, never by runtime
// environment. An arbitrary precompiled test binary cannot bypass that build
// by setting ROBOREV_QUALITY_CHILD alone.
var (
	qualityCompiledSelection       string
	qualityCompiledSourceInventory string
)

// This is the only model-evaluation launcher. It controls compiler flags and
// links a snapshot of selected source inputs into the evaluation child.
// Runtime declarations are checked against that seal before source enumeration.
func qualityBuildEvaluationBinary(ctx context.Context, root, harness, overlayFile, modfile, destination string) error {
	settings, err := qualityCurrentBuildSettings()
	if err != nil {
		return err
	}
	if err := qualitySupportedBuildSettings(settings); err != nil {
		return err
	}
	if err := qualityCheckHarness(harness); err != nil {
		return err
	}
	version, err := qualityCommand(ctx, root, "go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	if version != runtime.Version() {
		return fmt.Errorf("Go compiler version differs from running harness")
	}
	selection, err := qualitySelectionFingerprint(root, harness, overlayFile, modfile)
	if err != nil {
		return err
	}
	inputs, err := qualityCollectSourceInputs(ctx, root, overlayFile, modfile, harness)
	if err != nil {
		return err
	}
	if err := qualityValidateSourceInputs(ctx, inputs); err != nil {
		return err
	}
	sourceInventory, err := qualitySourceInventoryHash(inputs)
	if err != nil {
		return err
	}
	snapshotDir, err := os.MkdirTemp("", "roborev-quality-source-snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(snapshotDir)
	snapshotOverlay, err := qualityWriteSourceSnapshotOverlay(root, harness, overlayFile, snapshotDir, inputs)
	if err != nil {
		return err
	}
	snapshotModuleCache, err := qualityWriteModuleCacheSnapshot(snapshotDir, inputs)
	if err != nil {
		return err
	}
	snapshotModfile, err := qualityWriteSourceSnapshotModfile(snapshotDir, modfile, inputs)
	if err != nil {
		return err
	}
	ldflags := "-X=go.kenn.io/roborev/internal/review.qualityCompiledSelection=" + selection + " -X=go.kenn.io/roborev/internal/review.qualityCompiledSourceInventory=" + sourceInventory
	args := []string{"test", "-c", "-tags=qualityeval", "-mod=readonly", "-modfile=" + snapshotModfile, "-o", destination, "-ldflags=" + ldflags, "-overlay=" + snapshotOverlay}
	args = append(args, "./internal/review")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(qualityOfflineEnv(os.Environ()), qualitySelectionEnv(settings)...)
	cmd.Env = append(cmd.Env, "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	cmd.Env = append(cmd.Env, "GOMODCACHE="+snapshotModuleCache)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compile controlled evaluation child: %w: %s", err, output)
	}
	return nil
}

func qualityCheckCompilationBinding(root, harness, overlayFile, modfile string) error {
	settings, err := qualityCurrentBuildSettings()
	if err != nil {
		return err
	}
	if err := qualitySupportedBuildSettings(settings); err != nil {
		return err
	}
	if qualityCompiledSelection == "" {
		return fmt.Errorf("evaluation child was not built by the controlled launcher")
	}
	expected, err := qualitySelectionFingerprint(root, harness, overlayFile, modfile)
	if err != nil {
		return err
	}
	if qualityCompiledSelection != expected {
		return fmt.Errorf("runtime overlay/module selection differs from the actual evaluation-child build")
	}
	return nil
}
