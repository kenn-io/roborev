package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	acp "github.com/coder/acp-go-sdk"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func readTextFileWindow(path string, startLine int, limit *int, maxBytes int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var out strings.Builder
	var lineBuf bytes.Buffer
	currentLine := 0
	selectedLines := 0
	wroteLine := false
	endedWithNewline := false

	appendLine := func(line []byte) error {
		additionalBytes := len(line)
		if wroteLine {
			additionalBytes++
		}
		if out.Len()+additionalBytes > maxBytes {
			return fmt.Errorf("file content too large: exceeds max %d bytes", maxBytes)
		}
		if wroteLine {
			out.WriteByte('\n')
		}
		if len(line) > 0 {
			out.Write(line)
		}
		wroteLine = true
		selectedLines++
		return nil
	}

	lineBufferBudget := func() int {
		budget := maxBytes - out.Len()
		if wroteLine {
			budget--
		}
		return budget
	}

	appendLineChunk := func(chunk []byte) error {
		if len(chunk) == 0 {
			return nil
		}
		if lineBufferBudget() < lineBuf.Len()+len(chunk) {
			return fmt.Errorf("file content too large: exceeds max %d bytes", maxBytes)
		}
		_, err := lineBuf.Write(chunk)
		return err
	}

	for {
		chunk, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			if currentLine >= startLine && (limit == nil || selectedLines < *limit) {
				if err := appendLineChunk(chunk); err != nil {
					return "", err
				}
			}
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return "", readErr
		}
		if len(chunk) == 0 && readErr == io.EOF {
			break
		}

		hasTrailingNewline := len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		endedWithNewline = hasTrailingNewline

		if currentLine >= startLine && (limit == nil || selectedLines < *limit) {
			if hasTrailingNewline {
				chunk = chunk[:len(chunk)-1]
			}
			if err := appendLineChunk(chunk); err != nil {
				return "", err
			}
			if err := appendLine(lineBuf.Bytes()); err != nil {
				return "", err
			}
			lineBuf.Reset()
			if limit != nil && selectedLines >= *limit {
				return out.String(), nil
			}
		}
		currentLine++

		if readErr == io.EOF {
			return out.String(), nil
		}
	}

	// strings.Split preserves a trailing empty line when the file ends with '\n'.
	if endedWithNewline && currentLine >= startLine && (limit == nil || selectedLines < *limit) {
		if wroteLine {
			if out.Len()+1 > maxBytes {
				return "", fmt.Errorf("file content too large: exceeds max %d bytes", maxBytes)
			}
			out.WriteByte('\n')
		}
	}

	return out.String(), nil
}

func writeTextFileAtomically(path string, content []byte) error {
	parentDir := filepath.Dir(path)
	baseName := filepath.Base(path)

	existingInfo, statErr := os.Stat(path)
	pathExists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if pathExists {
		// Preserve write permission semantics for existing files.
		writableProbe, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_ = writableProbe.Close()
	}

	tempPerm := os.FileMode(0o644)
	if pathExists {
		tempPerm = existingInfo.Mode().Perm()
	}

	tempFile, err := createTempFileWithPerm(parentDir, baseName, tempPerm)
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	removeTemp := true
	defer func() {
		_ = tempFile.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := tempFile.Write(content); err != nil {
		return err
	}
	if pathExists {
		if err := tempFile.Chmod(existingInfo.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := tempFile.Sync(); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}

	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

func createTempFileWithPerm(parentDir, baseName string, perm os.FileMode) (*os.File, error) {
	for range 256 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, err
		}
		tempName := fmt.Sprintf(".%s.tmp-%x", baseName, suffix)
		tempPath := filepath.Join(parentDir, tempName)

		file, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return file, nil
		}
		if os.IsExist(err) {
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("failed to create temporary file in %s", parentDir)
}

func (c *acpClient) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {

	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.ReadTextFileResponse{}, err
	}

	// Validate input parameters
	if params.Path == "" {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path cannot be empty")
	}

	// Validate path to prevent directory traversal
	validatedPath, err := c.validateAndResolvePath(params.Path, false) // false = read operation
	if err != nil {
		return acp.ReadTextFileResponse{}, fmt.Errorf("failed to validate read path %s: %w", params.Path, err)
	}

	// Validate numeric parameters
	if params.Line != nil && *params.Line < 1 {
		return acp.ReadTextFileResponse{}, fmt.Errorf("invalid line number: %d (must be >= 1)", *params.Line)
	}
	if params.Limit != nil && *params.Limit < 0 {
		return acp.ReadTextFileResponse{}, fmt.Errorf("invalid limit: %d (must be >= 0)", *params.Limit)
	}

	// Validate that line and limit are reasonable to prevent resource exhaustion
	if params.Line != nil && *params.Line > 1000000 {
		return acp.ReadTextFileResponse{}, fmt.Errorf("line number too large: %d (max 1,000,000)", *params.Line)
	}
	if params.Limit != nil && *params.Limit > 1000000 {
		return acp.ReadTextFileResponse{}, fmt.Errorf("limit too large: %d (max 1,000,000)", *params.Limit)
	}

	var fileContent string

	startLine := 0
	if params.Line != nil {
		startLine = max(*params.Line-1, 0) // Convert to 0-based index
	}

	fileContent, err = readTextFileWindow(validatedPath, startLine, params.Limit, maxACPTextFileBytes)
	if err != nil {
		return acp.ReadTextFileResponse{}, fmt.Errorf("failed to read file %s: %w", validatedPath, err)
	}

	return acp.ReadTextFileResponse{
		Content: fileContent,
	}, nil
}

func (c *acpClient) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {

	// Validate session ID
	if err := c.validateSessionID(params.SessionId); err != nil {
		return acp.WriteTextFileResponse{}, err
	}

	// Enforce authorization at point of operation.
	if !c.agent.mutatingOperationsAllowed() {
		if c.agent.effectivePermissionMode() == c.agent.ReadOnlyMode {
			return acp.WriteTextFileResponse{}, fmt.Errorf("write operation not permitted in read-only mode")
		}
		return acp.WriteTextFileResponse{}, fmt.Errorf("write operation not permitted unless auto-approve mode is explicitly enabled")
	}

	// Validate input parameters
	if params.Path == "" {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path cannot be empty")
	}

	// Validate path to prevent directory traversal
	validatedPath, err := c.validateAndResolvePath(params.Path, true) // true = write operation
	if err != nil {
		return acp.WriteTextFileResponse{}, fmt.Errorf("failed to validate write path %s: %w", params.Path, err)
	}

	// Validate content size to prevent resource exhaustion
	if len(params.Content) > maxACPTextFileBytes {
		return acp.WriteTextFileResponse{}, fmt.Errorf("content too large: %d bytes (max %d)", len(params.Content), maxACPTextFileBytes)
	}

	// Write via temp file + rename to avoid validate-then-write races.
	err = writeTextFileAtomically(validatedPath, []byte(params.Content))
	if err != nil {
		return acp.WriteTextFileResponse{}, fmt.Errorf("failed to write file %s: %w", validatedPath, err)
	}

	return acp.WriteTextFileResponse{}, nil
}

func (c *acpClient) validateAndResolvePath(requestedPath string, forWrite bool) (string, error) {
	repoRoot := c.effectiveRepoRoot()
	if repoRoot == "" {
		return "", fmt.Errorf("repository root not set")
	}

	// Clean the path to remove any . or .. components
	cleanPath := filepath.Clean(requestedPath)

	// Get absolute path of repository root
	repoRootAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repository root path: %w", err)
	}

	// Get absolute path of the requested file
	absPath := cleanPath
	if !filepath.IsAbs(cleanPath) {
		absPath = filepath.Join(repoRootAbs, cleanPath)
	}

	// Clean the absolute path again to resolve any remaining . or .. components
	absPath = filepath.Clean(absPath)

	if forWrite {
		// For writes, validate parent directory only since file may not exist yet
		parentDir := filepath.Dir(absPath)
		resolvedParent, err := filepath.EvalSymlinks(parentDir)
		if err != nil {
			return "", fmt.Errorf("%w: failed to resolve parent directory symlinks for path %s: %w", ErrPathTraversal, requestedPath, err)
		}

		// Check if parent directory is within repository root
		resolvedRepoRoot, err := filepath.EvalSymlinks(repoRootAbs)
		if err != nil {
			return "", fmt.Errorf("failed to resolve repository root symlinks: %w", err)
		}

		// Normalize both paths for comparison
		resolvedParent = filepath.Clean(resolvedParent)
		resolvedRepoRoot = filepath.Clean(resolvedRepoRoot)

		// Check if resolved parent directory is within resolved repository root
		if !pathWithinRoot(resolvedParent, resolvedRepoRoot) {
			return "", fmt.Errorf("%w: path %s (parent resolved to %s) is outside repository root %s", ErrPathTraversal, requestedPath, resolvedParent, repoRoot)
		}

		// Append base filename without requiring it to exist.
		validatedPath := filepath.Join(resolvedParent, filepath.Base(absPath))

		// If the target exists and is a symlink, ensure its resolved destination
		// still stays within the repository root, then write through the resolved
		// target path to avoid symlink swap races.
		info, err := os.Lstat(validatedPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", fmt.Errorf("failed to inspect write target %s: %w", validatedPath, err)
			}
			return validatedPath, nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolvedTarget, err := filepath.EvalSymlinks(validatedPath)
			if err != nil {
				return "", fmt.Errorf("%w: failed to resolve write target symlink for path %s: %w", ErrPathTraversal, requestedPath, err)
			}
			resolvedTarget = filepath.Clean(resolvedTarget)
			if !pathWithinRoot(resolvedTarget, resolvedRepoRoot) {
				return "", fmt.Errorf("%w: path %s (symlink target resolved to %s) is outside repository root %s", ErrPathTraversal, requestedPath, resolvedTarget, repoRoot)
			}
			return resolvedTarget, nil
		}

		return validatedPath, nil
	}

	// For reads, keep strict validation requiring full path to exist
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// If we can't resolve symlinks (e.g., broken symlink or permission issue),
		// we treat this as an invalid path for security
		return "", fmt.Errorf("%w: failed to resolve symlinks for path %s: %w", ErrPathTraversal, requestedPath, err)
	}

	// Check if the resolved path is within the repository root
	// This prevents directory traversal attacks like ../../../etc/passwd
	// We need to ensure the path is within the repo root, accounting for symlinks
	resolvedRepoRoot, err := filepath.EvalSymlinks(repoRootAbs)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repository root symlinks: %w", err)
	}

	// Normalize both paths for comparison
	resolvedPath = filepath.Clean(resolvedPath)
	resolvedRepoRoot = filepath.Clean(resolvedRepoRoot)

	// Check if resolved path is within resolved repository root
	if !pathWithinRoot(resolvedPath, resolvedRepoRoot) {
		return "", fmt.Errorf("%w: path %s (resolved to %s) is outside repository root %s", ErrPathTraversal, requestedPath, resolvedPath, repoRoot)
	}

	return resolvedPath, nil
}

func pathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
