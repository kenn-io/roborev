package config

import (
	"testing"

	"github.com/BurntSushi/toml"
	tomlv2 "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexConfigOverrideArgsFlattensModelProviders(t *testing.T) {
	const tomlConfig = `
[agent.codex.config]
model_provider = "my-custom"

[agent.codex.config.model_providers.my-custom]
name = "My Provider"
base_url = "https://api.example.com/v1"
env_key = "MY_API_KEY"
wire_api = "responses"
request_max_retries = 5
`
	var cfg Config
	_, err := toml.Decode(tomlConfig, &cfg)
	require.NoError(t, err)

	assert.Equal(t, []string{
		`model_provider="my-custom"`,
		`model_providers.my-custom.base_url="https://api.example.com/v1"`,
		`model_providers.my-custom.env_key="MY_API_KEY"`,
		`model_providers.my-custom.name="My Provider"`,
		`model_providers.my-custom.request_max_retries=5`,
		`model_providers.my-custom.wire_api="responses"`,
	}, cfg.Agent.Codex.ConfigOverrideArgs())
}

func TestCodexConfigOverrideArgsEncodesScalarAndArrayTypes(t *testing.T) {
	cfg := CodexConfig{Config: map[string]any{
		"a_string": "x",
		"a_bool":   true,
		"an_int":   int64(7),
		"a_float":  1.5,
		"a_list":   []any{"one", "two"},
	}}

	assert.Equal(t, []string{
		`a_bool=true`,
		`a_float=1.5`,
		`a_list=["one", "two"]`,
		`a_string="x"`,
		`an_int=7`,
	}, cfg.ConfigOverrideArgs())
}

func TestCodexConfigOverrideArgsEmpty(t *testing.T) {
	assert := assert.New(t)
	assert.Nil(CodexConfig{}.ConfigOverrideArgs())
	assert.Nil(CodexConfig{Config: map[string]any{}}.ConfigOverrideArgs())
}

func TestCodexConfigOverrideArgsQuotesNonBareKeys(t *testing.T) {
	cfg := CodexConfig{Config: map[string]any{
		"model_providers": map[string]any{
			"foo.bar":   map[string]any{"base_url": "https://x"},
			"has space": map[string]any{"k": "v"},
		},
	}}

	assert.Equal(t, []string{
		`model_providers."foo.bar".base_url="https://x"`,
		`model_providers."has space".k="v"`,
	}, cfg.ConfigOverrideArgs())
}

func TestCodexConfigSurvivesSaveLoadRoundTrip(t *testing.T) {
	original := DefaultConfig()
	original.Agent.Codex.Config = map[string]any{
		"model_provider": "my-custom",
		"model_providers": map[string]any{
			"my-custom": map[string]any{
				"base_url": "https://api.example.com/v1",
				"env_key":  "MY_API_KEY",
			},
		},
	}

	encoded, err := tomlv2.Marshal(original)
	require.NoError(t, err)

	var reloaded Config
	_, err = toml.Decode(string(encoded), &reloaded)
	require.NoError(t, err)

	assert.Equal(t, original.Agent.Codex.ConfigOverrideArgs(), reloaded.Agent.Codex.ConfigOverrideArgs())
	assert.Equal(t, []string{
		`model_provider="my-custom"`,
		`model_providers.my-custom.base_url="https://api.example.com/v1"`,
		`model_providers.my-custom.env_key="MY_API_KEY"`,
	}, reloaded.Agent.Codex.ConfigOverrideArgs())
}
