---
title: Repository Management
description: Manage repositories tracked by roborev
---


Manage repositories tracked by roborev:

<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/cli-repo-list.svg" alt="roborev repo list output" loading="lazy" style="max-width: 480px">
</figure>

```bash
roborev repo list                     # List all repos with review counts
roborev repo show my-project          # Show repo details and stats
roborev repo rename old-name new-name # Rename display name
roborev repo move my-repo .           # Update root path after a directory move
roborev repo delete old-project       # Remove from tracking
roborev repo merge source target      # Merge reviews into another repo
```

## Subcommands

| Command | Description |
|---------|-------------|
| `repo list` | List all repositories with review counts |
| `repo show <name>` | Show detailed stats for a repository |
| `repo rename <old> <new>` | Rename a repository's display name |
| `repo move <name-or-path> <new-path>` | Update a repository's stored root path on disk |
| `repo delete <name>` | Remove repository from tracking |
| `repo merge <src> <dst>` | Move all reviews to another repo |

## Common Use Cases

### Rename for Clarity

The rename command is useful when you want a friendlier display name than the directory name:

```bash
roborev repo rename my-project-v2 "My Project"
```

### Moving or Renaming a Repository

If you rename or move a tracked repository's directory on disk (e.g. `mv ~/code/old-project ~/code/new-project`), use `repo move` to update the stored root path so existing jobs and reviews stay attached to the same entry:

```bash
# After 'mv old-project new-project', from inside the new directory:
roborev repo move old-project .

# Or specify the new path explicitly:
roborev repo move my-repo /Users/me/code/my-repo

# You can identify the repo by its old (now stale) path too:
roborev repo move /old/path /new/path
```

The first argument can be the database name or the current (or stale) repository path. The second argument is the new path; `.` resolves to the current directory's git repository root.

For repos with a git remote, the repository identity stays stable, so the move is transparent to PostgreSQL sync. For local-only repos, the identity changes to `local://<new-path>`. If the target path already belongs to another tracked repo, `repo move` refuses to overwrite it; use `repo merge` to combine the entries instead.

When you `repo rename` a repository whose stored root path no longer exists on disk, roborev now hints you toward `repo move` so reviews do not silently desync from the new directory.

### Consolidate Duplicates

The merge command consolidates duplicate entries (e.g., from symlinks or path changes):

```bash
# Reviews from /home/user/projects/myapp are stored under "myapp"
# Reviews from /home/user/work/myapp are stored under "myapp-1"
roborev repo merge myapp-1 myapp
```

### Clean Up Old Projects

```bash
roborev repo list                 # See all tracked repos
roborev repo delete old-project   # Remove one you no longer need
```

### Multiple Clones

You can have multiple local clones of the same remote repository (e.g., `~/project-main` and `~/project-feature`). Each clone is tracked separately in roborev while sharing the same repository identity for sync purposes.

When using [PostgreSQL Sync](/advanced/postgres-sync/), reviews from teammates are intelligently matched:
- If you have exactly one local clone with that identity, synced reviews appear there
- If you have multiple clones, a placeholder repo is created to avoid ambiguity

## How Repositories Are Tracked

roborev automatically creates a repository entry when you:

1. Run `roborev init` in a repo
2. Queue a review for a commit in a new repo
3. Run any roborev command in an untracked repo

The default display name is the directory name. You can customize this with:

```toml
# .roborev.toml in your repo
display_name = "My Custom Name"
```

## Git Worktrees

roborev fully supports git worktrees. Reviews are stored against the main repository, so commits made in any worktree are associated with the same review history. No configuration is needed.

```bash
# Create a worktree for a feature branch
git worktree add ~/projects/myapp-feature feature-branch
cd ~/projects/myapp-feature

# Reviews work normally, stored under the main repo
roborev review --branch
roborev refine
roborev tui
```

When running commands from a worktree:

- Reviews are stored using the **main repository path** (not the worktree path)
- The post-commit hook registers commits against the main repo root, even from linked worktrees
- The TUI shows all reviews for the repository regardless of which worktree you're in
- The TUI review screen displays the branch stored with the review, not the active worktree branch
- `roborev fix --open` filters to reviews reachable from the current worktree's HEAD
- `refine` correctly finds and addresses reviews for commits in any worktree

Without this, you'd get duplicate repository entries, scattered reviews, and confusion about which reviews belong to which code. With worktree support, everything is consolidated under the main repository.

If your repository uses `core.hooksPath` (common with Husky and other hook managers), roborev resolves relative paths against the main repository root so the post-commit hook fires correctly from linked worktrees.

## See Also

- [Configuration](/configuration/) - Per-repo and global settings
