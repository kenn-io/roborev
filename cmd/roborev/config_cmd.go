package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
)

// configScope represents which configuration source to read/write.
type configScope int

const (
	scopeMerged configScope = iota
	scopeGlobal
	scopeLocal
)

var errNotGitRepository = errors.New("not in a git repository")

// RepoResolver provides filesystem and git context.
type RepoResolver interface {
	RepoRoot() (string, error)
	WorkingDir() (string, error)
}

type defaultRepoResolver struct {
	ctx context.Context
}

func (r defaultRepoResolver) RepoRoot() (string, error) {
	return gitrepo.Root(r.ctx, ".")
}

func (defaultRepoResolver) WorkingDir() (string, error) {
	return os.Getwd()
}

// determineScope returns the scope based on --global and --local flags.
func determineScope(globalFlag, localFlag bool) (configScope, error) {
	if globalFlag && localFlag {
		return 0, fmt.Errorf("cannot use both --global and --local")
	}
	if globalFlag {
		return scopeGlobal, nil
	}
	if localFlag {
		return scopeLocal, nil
	}
	return scopeMerged, nil
}

// repoRoot returns the git repo root for the current directory.
// Returns ("", nil) when the repo root is optional and not found.
func repoRoot(resolver RepoResolver) (string, error) {
	if root, err := resolver.RepoRoot(); err == nil {
		return root, nil
	}

	root, err := findRepoRoot(resolver)
	if err != nil {
		if errors.Is(err, errNotGitRepository) {
			return "", nil
		}
		return "", err
	}
	return root, nil
}

// requireRepoRoot returns the git repo root or an actionable error.
func requireRepoRoot(resolver RepoResolver) (string, error) {
	root, err := repoRoot(resolver)
	if err != nil {
		return "", fmt.Errorf("determine repository root: %w", err)
	}
	if root == "" {
		return "", errNotGitRepository
	}
	return root, nil
}

// findRepoRoot walks up from the current directory to find a git repo root.
func findRepoRoot(resolver RepoResolver) (string, error) {
	dir, err := resolver.WorkingDir()
	if err != nil {
		return "", err
	}

	for {
		ok, err := hasGitMetadata(dir)
		if err != nil {
			return "", err
		}
		if ok {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errNotGitRepository
		}
		dir = parent
	}
}

func hasGitMetadata(dir string) (bool, error) {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return false, err
		}
		return strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:"), nil
	}

	headInfo, err := os.Stat(filepath.Join(gitPath, "HEAD"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !headInfo.IsDir(), nil
}

// getValueForScope retrieves a single config key value from the appropriate scope.
func getValueForScope(resolver RepoResolver, key string, scope configScope) (string, error) {
	switch scope {
	case scopeGlobal:
		cfg, err := config.LoadGlobal()
		if err != nil {
			return "", fmt.Errorf("load global config: %w", err)
		}
		val, err := config.GetConfigValue(cfg, key)
		if err != nil {
			return "", err
		}
		raw, _ := config.LoadRawGlobal()
		if raw == nil || !config.IsKeyInTOMLFile(raw, key) {
			return "", fmt.Errorf("key %q is not set in global config", key)
		}
		return val, nil

	case scopeLocal:
		repoPath, err := requireRepoRoot(resolver)
		if err != nil {
			return "", err
		}
		repoCfg, err := config.LoadRepoConfig(repoPath)
		if err != nil {
			return "", fmt.Errorf("load repo config: %w", err)
		}
		if repoCfg == nil {
			return "", fmt.Errorf("no local config (.roborev.toml) found")
		}
		val, err := config.GetConfigValue(repoCfg, key)
		if err != nil {
			return "", err
		}
		raw, _ := config.LoadRawRepo(repoPath)
		if raw == nil || !config.IsKeyInTOMLFile(raw, key) {
			return "", fmt.Errorf("key %q is not set in local config", key)
		}
		return val, nil

	default: // scopeMerged
		if !config.IsValidKey(key) {
			return "", fmt.Errorf("unknown config key: %q", key)
		}
		// Try local first, then global
		repoPath, err := repoRoot(resolver)
		if err != nil {
			return "", fmt.Errorf("determine repository root: %w", err)
		}
		if repoPath != "" {
			raw, err := config.LoadRawRepo(repoPath)
			if err != nil {
				return "", fmt.Errorf("load repo config: %w", err)
			}
			if raw != nil && config.IsKeyInTOMLFile(raw, key) {
				repoCfg, err := config.LoadRepoConfig(repoPath)
				if err != nil {
					return "", fmt.Errorf("load repo config: %w", err)
				}
				val, err := config.GetConfigValue(repoCfg, key)
				if err != nil {
					return "", err
				}
				return val, nil
			}
		}
		// Key not found in local config — check if it's a repo-only key
		if !config.IsGlobalKey(key) {
			return "", fmt.Errorf("key %q is not set in local config", key)
		}
		cfg, err := config.LoadGlobal()
		if err != nil {
			return "", fmt.Errorf("load global config: %w", err)
		}
		return config.GetConfigValue(cfg, key)
	}
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Get and set roborev configuration",
		Long:  "Inspect or modify roborev configuration values. Similar to git config.",
	}

	cmd.AddCommand(configGetCmd())
	cmd.AddCommand(configSetCmd())
	cmd.AddCommand(configListCmd())

	return cmd
}

func configGetCmd() *cobra.Command {
	var globalFlag, localFlag bool

	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Get a configuration value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, err := determineScope(globalFlag, localFlag)
			if err != nil {
				return err
			}
			val, err := getValueForScope(defaultRepoResolver{ctx: cmd.Context()}, args[0], scope)
			if err != nil {
				return err
			}
			fmt.Println(val)
			return nil
		},
	}
	cmd.Flags().BoolVar(&globalFlag, "global", false, "get from global config only")
	cmd.Flags().BoolVar(&localFlag, "local", false, "get from local repo config only")

	return cmd
}

func configSetCmd() *cobra.Command {
	var globalFlag, localFlag bool

	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]

			scope, err := determineScope(globalFlag, localFlag)
			if err != nil {
				return err
			}

			if scope == scopeGlobal {
				return setConfigKey(config.GlobalConfigPath(), key, value, true)
			}

			// Default (and --local): set in local config
			repoPath, err := requireRepoRoot(defaultRepoResolver{ctx: cmd.Context()})
			if err != nil {
				if errors.Is(err, errNotGitRepository) {
					return fmt.Errorf("%w (use --global for global config)", errNotGitRepository)
				}
				return err
			}
			localPath := filepath.Join(repoPath, ".roborev.toml")
			return setConfigKey(localPath, key, value, false)
		},
	}

	cmd.Flags().BoolVar(&globalFlag, "global", false, "set in global config")
	cmd.Flags().BoolVar(&localFlag, "local", false, "set in local repo config (default)")

	return cmd
}

func configListCmd() *cobra.Command {
	var globalFlag, localFlag, showOrigin bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configuration values",
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, err := determineScope(globalFlag, localFlag)
			if err != nil {
				return err
			}

			switch scope {
			case scopeGlobal:
				return listGlobalConfig()
			case scopeLocal:
				return listLocalConfig(defaultRepoResolver{ctx: cmd.Context()})
			default:
				return listMergedConfig(defaultRepoResolver{ctx: cmd.Context()}, showOrigin)
			}
		},
	}
	cmd.Flags().BoolVar(&globalFlag, "global", false, "list global config only")
	cmd.Flags().BoolVar(&localFlag, "local", false, "list local repo config only")
	cmd.Flags().BoolVar(&showOrigin, "show-origin", false, "show where each value comes from (global/local/default)")

	return cmd
}

func listGlobalConfig() error {
	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}
	raw, err := config.LoadRawGlobal()
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}
	printKeyValues(config.ListExplicitKeys(cfg, raw))
	return nil
}

func listLocalConfig(resolver RepoResolver) error {
	repoPath, err := requireRepoRoot(resolver)
	if err != nil {
		return err
	}
	repoCfg, err := config.LoadRepoConfig(repoPath)
	if err != nil {
		return fmt.Errorf("load repo config: %w", err)
	}
	if repoCfg == nil {
		return fmt.Errorf("no local config (.roborev.toml) found")
	}
	raw, err := config.LoadRawRepo(repoPath)
	if err != nil {
		return fmt.Errorf("load repo config: %w", err)
	}
	printKeyValues(config.ListExplicitKeys(repoCfg, raw))
	return nil
}

func listMergedConfig(resolver RepoResolver, showOrigin bool) error {
	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}
	rawGlobal, _ := config.LoadRawGlobal()

	var repoCfg *config.RepoConfig
	var rawRepo map[string]any
	repoPath, err := repoRoot(resolver)
	if err != nil {
		return fmt.Errorf("determine repository root: %w", err)
	}
	if repoPath != "" {
		repoCfg, err = config.LoadRepoConfig(repoPath)
		if err != nil {
			return fmt.Errorf("load repo config: %w", err)
		}
		rawRepo, err = config.LoadRawRepo(repoPath)
		if err != nil {
			return fmt.Errorf("load repo config: %w", err)
		}
	}

	kvos := config.MergedConfigWithOrigin(cfg, repoCfg, rawGlobal, rawRepo)
	if showOrigin {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, kvo := range kvos {
			val := kvo.Value
			if config.IsSensitiveKey(kvo.Key) {
				val = config.MaskValue(val)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", kvo.Origin, kvo.Key, val)
		}
		return w.Flush()
	}

	for _, kvo := range kvos {
		val := kvo.Value
		if config.IsSensitiveKey(kvo.Key) {
			val = config.MaskValue(val)
		}
		fmt.Printf("%s=%s\n", kvo.Key, val)
	}
	return nil
}

// printKeyValues prints key-value pairs, masking sensitive values
func printKeyValues(kvs []config.KeyValue) {
	for _, kv := range kvs {
		val := kv.Value
		if config.IsSensitiveKey(kv.Key) {
			val = config.MaskValue(val)
		}
		fmt.Printf("%s=%s\n", kv.Key, val)
	}
}

// setConfigKey sets a key in a TOML config file.
func setConfigKey(path, key, value string, isGlobal bool) error {
	_, err := validateKeyForScope(key, value, isGlobal)
	if err != nil {
		return err
	}

	if isGlobal {
		cfg, err := config.LoadGlobalFrom(path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if err := config.SetConfigValue(cfg, key, value); err != nil {
			return err
		}
		return config.SaveGlobalTo(path, cfg)
	}

	repoDir := filepath.Dir(path)
	repoCfg, err := config.LoadRepoConfig(repoDir)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if repoCfg == nil {
		repoCfg = &config.RepoConfig{}
	}
	if err := config.SetConfigValue(repoCfg, key, value); err != nil {
		return err
	}
	return config.SaveRepoConfigTo(path, repoCfg)
}

// validateKeyForScope validates a key against the appropriate config scope.
func validateKeyForScope(key, value string, isGlobal bool) (any, error) {
	if isGlobal {
		cfg := config.DefaultConfig()
		if err := config.SetConfigValue(cfg, key, value); err != nil {
			repoCfg := &config.RepoConfig{}
			if config.SetConfigValue(repoCfg, key, value) == nil {
				return nil, fmt.Errorf("key %q is a per-repo setting (use without --global, or set in .roborev.toml)", key)
			}
			return nil, err
		}
		return cfg, nil
	}

	repoCfg := &config.RepoConfig{}
	if err := config.SetConfigValue(repoCfg, key, value); err != nil {
		cfg := config.DefaultConfig()
		if config.SetConfigValue(cfg, key, value) == nil {
			return nil, fmt.Errorf("key %q is a global setting (use --global to set in %s)", key, config.GlobalConfigPath())
		}
		return nil, err
	}
	return repoCfg, nil
}

// setRawMapKey sets a value in a nested map using dot-separated keys.
func setRawMapKey(m map[string]any, key string, value any) {
	parts := strings.Split(key, ".")

	if len(parts) == 1 {
		m[parts[0]] = value
		return
	}

	// Navigate/create nested maps
	current := m
	for _, part := range parts[:len(parts)-1] {
		if sub, ok := current[part]; ok {
			if subMap, ok := sub.(map[string]any); ok {
				current = subMap
			} else {
				// Overwrite non-map value with new map
				newMap := make(map[string]any)
				current[part] = newMap
				current = newMap
			}
		} else {
			newMap := make(map[string]any)
			current[part] = newMap
			current = newMap
		}
	}

	current[parts[len(parts)-1]] = value
}
