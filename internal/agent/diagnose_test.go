package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/config"
)

func TestDiagnose(t *testing.T) {
	fakeCodex := writeAvailableACPCommand(t, "codex-wrapper")
	fakeACP := writeAvailableACPCommand(t, "goose-acp")
	t.Setenv("PATH", t.TempDir())

	tests := []struct {
		name      string
		agent     string
		cfg       *config.Config
		available bool
		unknown   bool
		command   string
		path      string
	}{
		{
			name:    "missing from PATH",
			agent:   "codex",
			cfg:     &config.Config{},
			command: "codex",
		},
		{
			name:      "command override",
			agent:     "codex",
			cfg:       &config.Config{CodexCmd: fakeCodex},
			available: true,
			command:   fakeCodex,
			path:      fakeCodex,
		},
		{
			name:    "alias resolves to the configured command",
			agent:   "claude",
			cfg:     &config.Config{},
			command: "claude",
		},
		{
			name:      "named ACP agent",
			agent:     "acp.goose",
			cfg:       &config.Config{ACP: config.ACPAgentConfigs{"goose": {Command: fakeACP}}},
			available: true,
			command:   fakeACP,
			path:      fakeACP,
		},
		{
			name:    "ACP name without a config table",
			agent:   "acp.missing",
			cfg:     &config.Config{},
			unknown: true,
		},
		{
			name:    "named ACP agent with missing command",
			agent:   "acp.goose",
			cfg:     &config.Config{ACP: config.ACPAgentConfigs{"goose": {Command: "goose-not-installed"}}},
			command: "goose-not-installed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			got := Diagnose(nil, tt.agent, tt.cfg)
			assert.Equal(tt.available, got.Available)
			assert.Equal(tt.command, got.Command)
			assert.Equal(tt.path, got.Path)
			assert.Equal(tt.unknown, got.Unknown)
			assert.Equal(!tt.available, got.Error != "", "error: %q", got.Error)
		})
	}
}

func TestDiagnoseAllSkipsTestAgent(t *testing.T) {
	for _, d := range DiagnoseAll(nil, &config.Config{}) {
		assert.NotEqual(t, "test", d.Name)
	}
}
