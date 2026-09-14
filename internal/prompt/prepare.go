package prompt

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// Prepare measures the complete prompt once content assembly is finished.
// The configured threshold selects inline text or a complete prompt file;
// neither path drops instructions, discussion, diffs, or review results.
func (b *Builder) Prepare(text string, target SnapshotTarget) (SnapshotResult, error) {
	// Diffs may contain legacy-encoded bytes. Normalize the assembled prompt
	// before measuring it; repository contents are left untouched.
	text = strings.ToValidUTF8(text, "\uFFFD")
	configRepo := target.ConfigRepoPath
	if configRepo == "" {
		configRepo = b.repoPath
	}
	if len(text) <= config.ResolveMaxPromptSize(configRepo, b.globalCfg) {
		return SnapshotResult{Prompt: text}, nil
	}
	repo, root, err := b.resolveSnapshotTarget(target)
	if err != nil {
		return SnapshotResult{}, err
	}
	file, cleanup, err := writeExternalSnapshot(repo, root, "prompt.md", text)
	if err != nil {
		return SnapshotResult{}, err
	}
	return SnapshotResult{
		Prompt:   fmt.Sprintf("Read the complete task prompt from %q and carry out its instructions. Read the file in full before starting.\n", file),
		FilePath: file,
		Cleanup:  cleanup,
	}, nil
}
