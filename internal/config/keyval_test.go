package config

import (
	"math"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toMap(kvs []KeyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

func toOriginMap(kvos []KeyValueOrigin) map[string]KeyValueOrigin {
	m := make(map[string]KeyValueOrigin, len(kvos))
	for _, kvo := range kvos {
		m[kvo.Key] = kvo
	}
	return m
}

func assertConfigValues(t *testing.T, actual []KeyValue, expected map[string]string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	m := toMap(actual)
	for k, want := range expected {
		got, ok := m[k]
		require.True(ok, "missing key %q", k)
		assert.Equal(want, got, "key %q", k)
	}
}

type expectedOrigin struct {
	Value  string
	Origin string
}

func assertOrigins(t *testing.T, actual []KeyValueOrigin, expected map[string]expectedOrigin) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	m := toOriginMap(actual)
	for k, want := range expected {
		got, ok := m[k]
		require.True(ok, "missing key %q", k)
		assert.Equal(want.Value, got.Value, "key %q value", k)
		assert.Equal(want.Origin, got.Origin, "key %q origin", k)
	}
}

func assertDeterministic(t *testing.T, fn func() string) {
	t.Helper()
	require := require.New(t)

	var prev string
	for i := range 20 {
		got := fn()
		if i == 0 {
			prev = got
			continue
		}
		require.Equal(prev, got, "non-deterministic output on iteration %d", i)

		prev = got
	}
}

func newComplexTestConfig() *Config {
	return &Config{
		DefaultAgent:       "codex",
		MaxWorkers:         4,
		ReviewContextCount: 3,
		Sync: SyncConfig{
			Enabled:     true,
			PostgresURL: "postgres://localhost/test",
		},
		CI: CIConfig{
			PollInterval: "10m",
			GitHubAppConfig: GitHubAppConfig{
				GitHubAppID:         12345,
				GitHubAppPrivateKey: "test-private-key",
			},
		},
	}
}

func TestGetConfigValue(t *testing.T) {
	cfg := newComplexTestConfig()

	tests := []struct {
		key  string
		want string
	}{
		{"default_agent", "codex"},
		{"max_workers", "4"},
		{"review_context_count", "3"},
		{"sync.enabled", "true"},
		{"sync.postgres_url", "postgres://localhost/test"},
		{"ci.poll_interval", "10m"},
		{"ci.github_app_id", "12345"},
		{"ci.github_app_private_key", "test-private-key"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			got, err := GetConfigValue(cfg, tt.key)
			require.NoError(err, "GetConfigValue(%q) error", tt.key)
			assert.Equal(tt.want, got)
		})
	}
}

func TestGetConfigValueUnknownKey(t *testing.T) {
	cfg := &Config{}
	_, err := GetConfigValue(cfg, "nonexistent")
	require.Error(t, err, "expected error for unknown key")
}

func TestSetConfigValue(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		val    string
		init   func() *Config
		verify func(*testing.T, *Config)
	}{
		{
			name: "set string field",
			key:  "default_agent",
			val:  "claude-code",
			verify: func(t *testing.T, c *Config) {
				assert.Equal(t, "claude-code", c.DefaultAgent)
			},
		},
		{
			name: "set int field",
			key:  "max_workers",
			val:  "8",
			verify: func(t *testing.T, c *Config) {
				assert.Equal(t, 8, c.MaxWorkers)
			},
		},
		{
			name: "set nested bool",
			key:  "sync.enabled",
			val:  "true",
			verify: func(t *testing.T, c *Config) {
				assert.True(t, c.Sync.Enabled)
			},
		},
		{
			name: "set embedded github app id",
			key:  "ci.github_app_id",
			val:  "98765",
			verify: func(t *testing.T, c *Config) {
				assert.EqualValues(t, 98765, c.CI.GitHubAppID)
			},
		},
		{
			name: "set embedded github app private key",
			key:  "ci.github_app_private_key",
			val:  "private-key-data",
			verify: func(t *testing.T, c *Config) {
				assert.Equal(t, "private-key-data", c.CI.GitHubAppPrivateKey)
			},
		},
		{
			name: "set bool ptr",
			key:  "allow_unsafe_agents",
			val:  "true",
			verify: func(t *testing.T, c *Config) {
				assert.False(t, c.AllowUnsafeAgents == nil || !*c.AllowUnsafeAgents)
			},
		},
		{
			name: "set slice",
			key:  "ci.repos",
			val:  "org/repo1,org/repo2",
			verify: func(t *testing.T, c *Config) {
				assert.False(t, len(c.CI.Repos) != 2 || c.CI.Repos[0] != "org/repo1" || c.CI.Repos[1] != "org/repo2")
			},
		},
		{
			name: "set slice from nil",
			key:  "ci.repos",
			val:  "org/repo1,org/repo2",
			init: func() *Config { return &Config{} },
			verify: func(t *testing.T, c *Config) {
				assert.False(t, len(c.CI.Repos) != 2 || c.CI.Repos[0] != "org/repo1" || c.CI.Repos[1] != "org/repo2")
			},
		},
		{
			name: "set slice empty",
			key:  "ci.repos",
			val:  "",
			verify: func(t *testing.T, c *Config) {
				assert.Empty(t, c.CI.Repos)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			var cfg *Config
			if tt.init != nil {
				cfg = tt.init()
			} else {
				cfg = &Config{
					CI: CIConfig{
						Repos: []string{"initial"},
					},
				}
			}
			err := SetConfigValue(cfg, tt.key, tt.val)
			require.NoError(err, "SetConfigValue(%q, %q) error", tt.key, tt.val)
			tt.verify(t, cfg)
		})
	}
}

func TestSetConfigValueMultipleKeys(t *testing.T) {
	require := require.New(t)

	cfg := &Config{}
	updates := []struct {
		key string
		val string
	}{
		{key: "default_agent", val: "claude-code"},
		{key: "max_workers", val: "8"},
		{key: "sync.enabled", val: "true"},
		{key: "ci.github_app_id", val: "98765"},
		{key: "ci.github_app_private_key", val: "private-key-data"},
	}

	for _, update := range updates {
		require.NoError(SetConfigValue(cfg, update.key, update.val), "SetConfigValue(%q, %q) error", update.key, update.val)
	}

	assertConfigValues(t, ListConfigKeys(cfg), map[string]string{
		"default_agent":             "claude-code",
		"max_workers":               "8",
		"sync.enabled":              "true",
		"ci.github_app_id":          "98765",
		"ci.github_app_private_key": "private-key-data",
	})
}

func TestSetConfigValueInvalidType(t *testing.T) {
	cfg := &Config{}
	require.Error(t, SetConfigValue(cfg, "max_workers", "notanumber"), "expected error for invalid integer")
}

func TestListConfigKeys(t *testing.T) {
	cfg := newComplexTestConfig()

	cfg.CI.GitHubAppPrivateKey = "private-key-data"

	assertConfigValues(t, ListConfigKeys(cfg), map[string]string{
		"default_agent":             "codex",
		"max_workers":               "4",
		"review_context_count":      "3",
		"sync.enabled":              "true",
		"sync.postgres_url":         "postgres://localhost/test",
		"ci.poll_interval":          "10m",
		"ci.github_app_id":          "12345",
		"ci.github_app_private_key": "private-key-data",
	})
}

func TestListConfigKeysRepo(t *testing.T) {
	cfg := &RepoConfig{
		Agent:            "claude-code",
		ReviewGuidelines: "Be thorough",
	}

	assertConfigValues(t, ListConfigKeys(cfg), map[string]string{
		"agent":             "claude-code",
		"review_guidelines": "Be thorough",
	})
}

func TestListConfigKeysIncludesComplexNonZeroFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := &Config{
		Hooks: []HookConfig{
			{
				Event:   "review.failed",
				Command: "echo failed",
				Type:    "command",
			},
		},
		Sync: SyncConfig{
			RepoNames: map[string]string{
				"org/repo": "my-project",
			},
		},
		CI: CIConfig{
			GitHubAppConfig: GitHubAppConfig{
				GitHubAppInstallations: map[string]int64{
					"org": 1234,
				},
			},
		},
	}

	found := toMap(ListConfigKeys(cfg))

	got, ok := found["sync.repo_names"]
	require.True(ok, "missing sync.repo_names")
	assert.Contains(got, "org/repo:my-project")
	got, ok = found["ci.github_app_installations"]
	require.True(ok, "missing ci.github_app_installations")
	assert.Contains(got, "org:1234")
	got, ok = found["hooks"]
	require.True(ok, "missing hooks")
	assert.Contains(got, "review.failed")
}

func TestMergedConfigWithOrigin(t *testing.T) {
	tests := []struct {
		name      string
		global    *Config
		repo      *RepoConfig
		rawGlobal map[string]any
		rawRepo   map[string]any
		want      map[string]expectedOrigin
		verify    func(*testing.T, []KeyValueOrigin)
	}{
		{
			name: "both local and global set",
			global: func() *Config {
				g := DefaultConfig()
				g.DefaultAgent = "gemini"
				return g
			}(),
			repo:      &RepoConfig{Agent: "claude-code"},
			rawGlobal: map[string]any{"default_agent": "gemini"},
			rawRepo:   map[string]any{"agent": "claude-code"},
			want: map[string]expectedOrigin{
				"default_agent": {Value: "gemini", Origin: "global"},
				"agent":         {Value: "claude-code", Origin: "local"},
			},
			verify: func(t *testing.T, kvos []KeyValueOrigin) {
				assert := assert.New(t)
				found := toOriginMap(kvos)
				if kvo, ok := found["max_workers"]; ok {
					assert.Equal("default", kvo.Origin, "max_workers origin")
				}
			},
		},
		{
			name: "local overrides global",
			global: func() *Config {
				g := DefaultConfig()
				g.ReviewContextCount = 5
				return g
			}(),
			repo:      &RepoConfig{ReviewContextCount: 10},
			rawGlobal: map[string]any{"review_context_count": int64(5)},
			rawRepo:   map[string]any{"review_context_count": int64(10)},
			want: map[string]expectedOrigin{
				"review_context_count": {Value: "10", Origin: "local"},
			},
		},
		{
			name: "shows all origins",
			global: func() *Config {
				g := DefaultConfig()
				g.DefaultAgent = "gemini"
				return g
			}(),
			rawGlobal: map[string]any{"default_agent": "gemini"},
			want: map[string]expectedOrigin{
				"default_agent": {Value: "gemini", Origin: "global"},
			},
			verify: func(t *testing.T, kvos []KeyValueOrigin) {
				assert := assert.New(t)
				found := toOriginMap(kvos)
				assert.Equal("default", found["max_workers"].Origin, "max_workers origin")
			},
		},
		{
			name: "includes complex fields",
			global: func() *Config {
				g := DefaultConfig()
				g.Sync.RepoNames = map[string]string{"org/repo": "my-project"}
				g.CI.GitHubAppInstallations = map[string]int64{"org": 1234}
				return g
			}(),
			rawGlobal: map[string]any{
				"sync": map[string]any{
					"repo_names": map[string]any{"org/repo": "my-project"},
				},
				"ci": map[string]any{
					"github_app_installations": map[string]any{"org": int64(1234)},
				},
			},
			want: map[string]expectedOrigin{
				"sync.repo_names":             {Value: "org/repo:my-project", Origin: "global"},
				"ci.github_app_installations": {Value: "org:1234", Origin: "global"},
			},
		},
		{
			name:   "omits unset complex fields",
			global: DefaultConfig(),
			verify: func(t *testing.T, kvos []KeyValueOrigin) {
				assert := assert.New(t)
				found := toOriginMap(kvos)
				_, ok := found["hooks"]
				assert.False(ok, "merged output should not include unset hooks")
				_, ok = found["sync.repo_names"]
				assert.False(ok, "merged output should not include unset sync.repo_names")
				_, ok = found["ci.github_app_installations"]
				assert.False(ok, "merged output should not include unset ci.github_app_installations")
			},
		},
		{
			name:      "explicit global matching default shows global origin",
			global:    DefaultConfig(),
			rawGlobal: map[string]any{"max_workers": int64(4)},
			verify: func(t *testing.T, kvos []KeyValueOrigin) {
				assert := assert.New(t)
				require := require.New(t)
				found := toOriginMap(kvos)
				kvo, ok := found["max_workers"]
				require.True(ok, "expected max_workers in output")
				assert.Equal("global", kvo.Origin, "max_workers origin")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kvos := MergedConfigWithOrigin(tt.global, tt.repo, tt.rawGlobal, tt.rawRepo)
			if tt.want != nil {
				assertOrigins(t, kvos, tt.want)
			}
			if tt.verify != nil {
				tt.verify(t, kvos)
			}
		})
	}
}

func TestIsConfigValueSet(t *testing.T) {
	assert := assert.New(t)

	cfg := &Config{
		DefaultAgent: "codex",
		MaxWorkers:   4,
	}

	assert.True(IsConfigValueSet(cfg, "default_agent"))
	assert.True(IsConfigValueSet(cfg, "max_workers"))
	assert.False(IsConfigValueSet(cfg, "cursor_cmd"))
	assert.False(IsConfigValueSet(cfg, "nonexistent"))
}

func TestFormatMapDeterministic(t *testing.T) {
	assert := assert.New(t)

	cfg := &Config{
		Sync: SyncConfig{
			RepoNames: map[string]string{
				"b/repo": "bravo",
				"a/repo": "alpha",
				"c/repo": "charlie",
			},
		},
	}

	assertDeterministic(t, func() string {
		kvs := ListConfigKeys(cfg)
		found := toMap(kvs)
		return found["sync.repo_names"]
	})

	kvs := ListConfigKeys(cfg)
	found := toMap(kvs)
	want := "a/repo:alpha,b/repo:bravo,c/repo:charlie"
	assert.Equal(want, found["sync.repo_names"])
}

type collidingKey int

func (k collidingKey) String() string { return "same" }

func TestFormatMapCollidingKeys(t *testing.T) {
	assert := assert.New(t)

	m := map[collidingKey]string{
		collidingKey(1): "alpha",
		collidingKey(2): "bravo",
		collidingKey(3): "charlie",
	}

	assertDeterministic(t, func() string {
		return formatMap(reflect.ValueOf(m))
	})

	result := formatMap(reflect.ValueOf(m))
	for _, val := range []string{"alpha", "bravo", "charlie"} {
		assert.Contains(result, val)
	}

	want := "same:alpha,same:bravo,same:charlie"
	assert.Equal(want, result)
}

type fullyCollidingKey int

func (k fullyCollidingKey) String() string   { return "same" }
func (k fullyCollidingKey) GoString() string { return "same" }

func TestFormatMapFullyCollidingKeys(t *testing.T) {
	assert := assert.New(t)

	m := map[fullyCollidingKey]string{
		fullyCollidingKey(10): "x",
		fullyCollidingKey(20): "y",
		fullyCollidingKey(30): "z",
	}

	assertDeterministic(t, func() string {
		return formatMap(reflect.ValueOf(m))
	})

	want := "same:x,same:y,same:z"

	got := formatMap(reflect.ValueOf(m))
	assert.Equal(want, got)
}

func TestIsValidKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"default_agent", true},
		{"agent", true},
		{"agent.codex.disable_review_skills", true},
		{"agent.codex.ignore_review_user_config", true},
		{"max_workers", true},
		{"sync.enabled", true},
		{"sync.repo_names", true},
		{"ci.github_app_id", true},
		{"ci.github_app_private_key", true},
		{"hooks", true},
		{"nonexistent", false},
		{"fake.key", false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			assert := assert.New(t)
			got := IsValidKey(tt.key)
			assert.Equal(tt.want, got)
		})
	}
}

func TestIsSensitiveKey(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsSensitiveKey("ci.github_app_private_key"))
	assert.False(IsSensitiveKey("ci.github_app_id"))
	assert.True(IsSensitiveKey("hooks.url"))
}

func TestIsGlobalKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"default_agent", true},
		{"max_workers", true},
		{"agent.codex.disable_review_skills", true},
		{"agent.codex.ignore_review_user_config", true},
		{"sync.enabled", true},
		{"sync.repo_names", true},
		{"hooks", true},
		{"agent", false},
		{"sync", false},
		{"review_guidelines", true},
		{"nonexistent", false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			assert := assert.New(t)
			got := IsGlobalKey(tt.key)
			assert.Equal(tt.want, got)
		})
	}
}

func TestReviewGuidelinesValidInGlobalAndRepoConfig(t *testing.T) {
	assert := assert.New(t)
	assert.NoError(SetConfigValue(&Config{}, "review_guidelines", "Global rule"))
	assert.NoError(SetConfigValue(&RepoConfig{}, "review_guidelines", "Repo rule"))
	assert.NoError(SetConfigValue(&RepoConfig{}, "review_guidelines_supersede_global", "true"))
}

func TestListExplicitKeys(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *Config
		raw    map[string]any
		verify func(*testing.T, []KeyValue)
	}{
		{
			name: "only includes raw keys",
			cfg: func() *Config {
				c := DefaultConfig()
				c.MaxWorkers = 8
				return c
			}(),
			raw: map[string]any{"max_workers": int64(8)},
			verify: func(t *testing.T, kvs []KeyValue) {
				assert := assert.New(t)
				found := toMap(kvs)
				_, ok := found["max_workers"]
				assert.True(ok, "expected max_workers to be listed (explicitly in TOML)")
				_, ok = found["default_agent"]
				assert.False(ok, "default_agent should NOT be listed (not in raw TOML)")
			},
		},
		{
			name: "includes zero values",
			cfg: &Config{
				MaxWorkers: 0,
				Sync:       SyncConfig{Enabled: false},
			},
			raw: map[string]any{
				"max_workers": int64(0),
				"sync":        map[string]any{"enabled": false},
			},
			verify: func(t *testing.T, kvs []KeyValue) {
				assert := assert.New(t)
				require := require.New(t)
				found := toMap(kvs)
				got, ok := found["max_workers"]
				require.True(ok, "expected max_workers to be listed (explicit zero in TOML)")
				assert.Equal("0", got)
				got, ok = found["sync.enabled"]
				require.True(ok, "expected sync.enabled to be listed (explicit false in TOML)")
				assert.Equal("false", got)
			},
		},
		{
			name: "includes empty values",
			cfg: &Config{
				DefaultModel: "",
				CI:           CIConfig{Repos: []string{}},
				Sync:         SyncConfig{RepoNames: map[string]string{}},
			},
			raw: map[string]any{
				"default_model": "",
				"ci":            map[string]any{"repos": []any{}},
				"sync":          map[string]any{"repo_names": map[string]any{}},
			},
			verify: func(t *testing.T, kvs []KeyValue) {
				assert := assert.New(t)
				found := toMap(kvs)
				_, ok := found["default_model"]
				assert.True(ok, "expected default_model to be listed (explicit empty string in TOML)")
				_, ok = found["ci.repos"]
				assert.True(ok, "expected ci.repos to be listed (explicit empty slice in TOML)")
				_, ok = found["sync.repo_names"]
				assert.True(ok, "expected sync.repo_names to be listed (explicit empty map in TOML)")
			},
		},
		{
			name: "nil raw",
			cfg:  DefaultConfig(),
			raw:  nil,
			verify: func(t *testing.T, kvs []KeyValue) {
				assert := assert.New(t)
				assert.Empty(kvs, "expected empty result for nil raw")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kvs := ListExplicitKeys(tt.cfg, tt.raw)
			tt.verify(t, kvs)
		})
	}
}

func TestDetermineOriginExplicitGlobalMatchingDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	rawGlobal := map[string]any{
		"max_workers": int64(4),
	}
	origin, ok := determineOrigin("max_workers", "4", "4", rawGlobal)
	require.True(ok, "expected key to be included in output")
	assert.Equal("global", origin)
}

func TestFormatMapNilInterfaceKeys(t *testing.T) {
	assert := assert.New(t)

	m := map[any]string{
		nil:   "null-val",
		"abc": "string-val",
		42:    "int-val",
	}

	assertDeterministic(t, func() string {
		return formatMap(reflect.ValueOf(m))
	})

	result := formatMap(reflect.ValueOf(m))
	for _, val := range []string{"null-val", "string-val", "int-val"} {
		assert.Contains(result, val)
	}
	assert.Contains(result, "<nil>:null-val")
}

func TestFormatMapNaNFloatKeys(t *testing.T) {
	assert := assert.New(t)

	nan1 := math.NaN()
	nan2 := math.Float64frombits(math.Float64bits(nan1) ^ 1)

	m := map[float64]string{
		nan1: "first",
		nan2: "second",
		1.0:  "one",
	}

	assertDeterministic(t, func() string {
		return formatMap(reflect.ValueOf(m))
	})

	result := formatMap(reflect.ValueOf(m))
	for _, val := range []string{"first", "second", "one"} {
		assert.Contains(result, val)
	}
}

func TestCompareKeysNilInterfaces(t *testing.T) {
	assert := assert.New(t)

	var i1, i2 any
	v1 := reflect.ValueOf(&i1).Elem()
	v2 := reflect.ValueOf(&i2).Elem()
	assert.Equal(0, compareKeys(v1, v2))

	i2 = 42
	v2 = reflect.ValueOf(&i2).Elem()
	assert.Equal(-1, compareKeys(v1, v2))

	assert.Equal(1, compareKeys(v2, v1))
}
