package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ciCheckCodex struct {
	Command   string
	ReadPaths []string
	PathDir   string
}

func resolveCICheckCodex(ctx context.Context, dir, command string) (resolved ciCheckCodex, err error) {
	codex, err := resolveCICheckCommand(ctx, dir, command)
	if err != nil {
		return resolved, fmt.Errorf("find checker codex: %w", err)
	}
	real, err := filepath.EvalSymlinks(codex)
	if err != nil {
		return resolved, err
	}
	// Native Codex loads sibling helpers. npm launchers load their package.
	readPaths := []string{filepath.Dir(real)}
	pathDir := ""
	launcher, openErr := os.Open(real)
	if openErr != nil {
		err = openErr
		return
	}
	var header [2]byte
	_, readErr := io.ReadFull(launcher, header[:])
	closeErr := launcher.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		err = readErr
		return
	}
	if closeErr != nil {
		err = closeErr
		return
	}
	if string(header[:]) == "#!" {
		var node string
		node, err = resolveCICheckCommand(ctx, dir, "node")
		if err != nil {
			err = fmt.Errorf("find checker node: %w", err)
			return
		}
		if filepath.Ext(real) == ".js" && filepath.Base(filepath.Dir(real)) == "bin" {
			readPaths[0] = filepath.Dir(filepath.Dir(real))
			// Resolve the optional native package as the npm launcher does.
			// It may be a sibling package or a package-manager symlink.
			resolve := exec.CommandContext(ctx, node, "--input-type=module", "-e", `
import { createRequire } from 'node:module';
import { dirname } from 'node:path';
const require = createRequire(process.argv[1]);
try {
  console.log(dirname(require.resolve('@openai/codex-' + process.platform + '-' + process.arch + '/package.json')));
} catch (error) {
  if (error.code !== 'MODULE_NOT_FOUND') throw error;
  // The launcher's bundled vendor directory is already granted.
}
`, real)
			resolve.Dir = dir
			var nativePackage []byte
			nativePackage, err = resolve.Output()
			if err != nil {
				err = fmt.Errorf("resolve checker native package: %w", err)
				return
			}
			if path := strings.TrimSpace(string(nativePackage)); path != "" {
				readPaths = append(readPaths, path)
			}
		}
		nodeReal, resolveErr := filepath.EvalSymlinks(node)
		if resolveErr != nil {
			err = resolveErr
			return
		}
		readPaths = append(readPaths, filepath.Dir(nodeReal))
		pathDir = filepath.Dir(node)
	}
	if _, statErr := os.Stat("/etc/codex"); statErr == nil {
		readPaths = append(readPaths, "/etc/codex")
	} else if !os.IsNotExist(statErr) {
		err = statErr
		return
	}
	resolved = ciCheckCodex{Command: codex, ReadPaths: readPaths, PathDir: pathDir}
	return
}

func resolveCICheckCommand(ctx context.Context, dir, name string) (string, error) {
	command, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	command, err = filepath.Abs(command)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(command)
	if err != nil {
		return "", err
	}
	// Resolve mise outside the checkout so repository config cannot select a tool.
	if filepath.Base(real) == "mise" {
		which := exec.CommandContext(ctx, real, "which", filepath.Base(name))
		which.Dir = dir
		path, err := which.Output()
		if err != nil {
			return "", err
		}
		return filepath.Abs(strings.TrimSpace(string(path)))
	}
	return command, nil
}
