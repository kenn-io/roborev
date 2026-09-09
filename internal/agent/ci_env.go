package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ciReviewContextKey struct{}

// WithCIReview marks agent work whose children must not inherit Git publishing
// credentials or helpers. The parent keeps its environment for comment posting.
func WithCIReview(ctx context.Context) (context.Context, func(), error) {
	dir, err := os.MkdirTemp("", "roborev-ci-git-")
	if err != nil {
		return nil, nil, fmt.Errorf("prepare CI Git environment: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	// A real empty file also works with Git for Windows, where NUL is not a
	// usable global-config path. The directory has no hooks or gh auth files.
	if err := os.WriteFile(filepath.Join(dir, "gitconfig"), nil, 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("prepare CI Git config: %w", err)
	}
	return context.WithValue(ctx, ciReviewContextKey{}, dir), cleanup, nil
}

func ciReviewDir(ctx context.Context) string {
	dir, _ := ctx.Value(ciReviewContextKey{}).(string)
	return dir
}

func isCIReview(ctx context.Context) bool { return ciReviewDir(ctx) != "" }

// ciReviewEnv removes inherited Git identity, configuration, transport helpers,
// and forge credentials. Provider authentication remains available, including
// Copilot's separate COPILOT_GITHUB_TOKEN with only Copilot Requests permission.
// These are subprocess defaults; tool permissions still govern agent actions.
func ciReviewEnv(env []string, dir string) []string {
	clean := StripUntrustedEnv(env)
	clean = filterEnv(clean, "EMAIL", "GPG_AGENT_INFO", "GNUPGHOME",
		"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"GH_CONFIG_DIR", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	filtered := make([]string, 0, len(clean))
	for _, entry := range clean {
		key, _, _ := strings.Cut(entry, "=")
		if envKeysAreCaseInsensitive {
			key = strings.ToUpper(key)
		}
		if strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "SSH_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = append(filtered,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"),
		"GH_CONFIG_DIR="+dir,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_ALLOW_PROTOCOL=",
		"GIT_CONFIG_COUNT=6",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1="+dir,
		"GIT_CONFIG_KEY_2=core.askPass", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=user.useConfigOnly", "GIT_CONFIG_VALUE_3=true",
		"GIT_CONFIG_KEY_4=user.name", "GIT_CONFIG_VALUE_4=",
		"GIT_CONFIG_KEY_5=user.email", "GIT_CONFIG_VALUE_5=",
	)
	return filtered
}
