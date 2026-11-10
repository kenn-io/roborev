package daemon

import (
	"os/exec"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// costSourceHealth checks availability only: it never executes agentsview,
// reads credentials or session data, or contacts a configured HTTP provider.
func costSourceHealth(cfg *config.Config) storage.ComponentHealth {
	health := storage.ComponentHealth{Name: "cost", Healthy: true}
	if cfg.AgentsView.URL != "" || cfg.Cost.Endpoint != "" {
		return health
	}
	if _, err := exec.LookPath("agentsview"); err == nil {
		return health
	}
	health.Healthy = false
	health.Message = "no usage source: agentsview is not on the daemon PATH; install agentsview or configure agentsview.url or cost.endpoint to record token usage and cost"
	return health
}
