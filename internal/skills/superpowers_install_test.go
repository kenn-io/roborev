package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallSuperpowersWorkflow(t *testing.T) {
	for _, agent := range Agents() {
		t.Run(string(agent), func(t *testing.T) {
			destination := t.TempDir()
			result, err := InstallToPath(agent, destination, nil)
			require.NoError(t, err)
			assert.Contains(t, result.Installed, "roborev-superpowers")
			content, err := os.ReadFile(filepath.Join(destination, "roborev-superpowers", "SKILL.md"))
			require.NoError(t, err)
			name, _ := parseFrontmatter(content)
			assert.Equal(t, "roborev-superpowers", name)
		})
	}
}
