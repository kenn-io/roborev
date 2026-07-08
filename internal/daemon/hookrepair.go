package daemon

import (
	"context"
	"log"
	"os"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/storage"
)

// repairRegisteredHooks reconciles roborev-managed git hooks in registered
// repos against the running binary. Upgrades performed by older updaters or
// outside `roborev update` (install scripts, manual downloads) leave hooks
// pointing at a stale binary; repairing at daemon startup self-heals them
// because the post-upgrade daemon always runs the new binary.
func repairRegisteredHooks(ctx context.Context, repos []storage.Repo) {
	// A daemon run from `go run` or a test binary would bake an ephemeral
	// path into hooks; leave hooks alone in that case.
	if exe, err := os.Executable(); err != nil || kitdaemon.IsEphemeralExecutable(exe) {
		return
	}
	resolution, err := githook.ResolveRoborevPath("")
	if err != nil {
		log.Printf("Warning: hook repair skipped: %v", err)
		return
	}
	for _, repo := range repos {
		repairRepoHooksAtStartup(ctx, repo.RootPath, resolution.Path)
	}
}

func repairRepoHooksAtStartup(ctx context.Context, root, binaryPath string) {
	inside, err := githook.HooksDirInsideWorktree(ctx, root)
	if err != nil {
		// Registered repo may have been deleted; nothing to repair.
		return
	}
	if inside {
		// core.hooksPath points into the working tree, which may hold
		// tracked files the daemon must not modify. Warn instead.
		if githook.NeedsUpgrade(ctx, root, "post-commit", githook.PostCommitVersionMarker) {
			log.Printf("Warning: outdated post-commit hook in %s -- run 'roborev init' to upgrade", root)
		}
		if githook.NeedsUpgrade(ctx, root, "post-rewrite", githook.PostRewriteVersionMarker) ||
			githook.Missing(ctx, root, "post-rewrite") {
			log.Printf("Warning: missing or outdated post-rewrite hook in %s -- run 'roborev init' to install", root)
		}
		return
	}
	if _, err := githook.RepairRepoHooks(ctx, root, binaryPath); err != nil {
		log.Printf("Warning: failed to repair hooks in %s: %v", root, err)
	}
	if githook.Missing(ctx, root, "post-rewrite") {
		log.Printf("Warning: missing post-rewrite hook in %s -- run 'roborev init' to install", root)
	}
}
