package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
)

// gitSHAPattern matches a full hex git SHA: 40 chars (SHA-1)
// or 64 chars (SHA-256).
var gitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func parseRemapPairs(r io.Reader) ([][2]string, error) {
	var pairs [][2]string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pairs = append(pairs, [2]string{fields[0], fields[1]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return pairs, nil
}

func remapCmd() *cobra.Command {
	var quiet bool

	cmd := &cobra.Command{
		Use:    "remap",
		Short:  "Remap review jobs after a rebase",
		Hidden: true,
		Long: `Reads old-sha/new-sha pairs from stdin (one per line,
space-separated) and updates review jobs to point at the
new commits. Called automatically by the post-rewrite hook.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			gitCwd, err := gitrepo.Root(ctx, ".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}
			repoRoot, err := gitrepo.MainRoot(ctx, ".")
			if err != nil {
				repoRoot = gitCwd
			}

			pairs, err := parseRemapPairs(os.Stdin)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}

			var mappings []daemon.RemapMapping
			for _, pair := range pairs {
				oldSHA, newSHA := pair[0], pair[1]

				if !gitSHAPattern.MatchString(oldSHA) ||
					!gitSHAPattern.MatchString(newSHA) {
					continue
				}

				oldPatchID := git.GetPatchID(gitCwd, oldSHA)
				newPatchID := git.GetPatchID(gitCwd, newSHA)

				// Skip if either has no patch-id or they differ
				if oldPatchID == "" || newPatchID == "" {
					continue
				}
				if oldPatchID != newPatchID {
					continue
				}

				info, err := git.GetCommitInfo(gitCwd, newSHA)
				if err != nil {
					continue
				}

				mappings = append(mappings, daemon.RemapMapping{
					OldSHA:    oldSHA,
					NewSHA:    newSHA,
					PatchID:   newPatchID,
					Author:    info.Author,
					Subject:   info.Subject,
					Timestamp: info.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
				})
			}

			if len(mappings) == 0 {
				if !quiet {
					fmt.Println("No mappings to remap")
				}
				return nil
			}

			ep := getDaemonEndpoint()
			client := daemon.NewHTTPClient(ep)

			result, err := client.Remap(daemon.RemapRequest{
				RepoPath: repoRoot,
				Mappings: mappings,
			})
			if err != nil {
				if !quiet {
					fmt.Fprintf(os.Stderr, "remap failed: %v\n", err)
				}
				return nil // Don't fail the hook
			}

			if !quiet {
				fmt.Printf("Remapped %d review(s), skipped %d\n",
					result.Remapped, result.Skipped)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress output")

	return cmd
}
