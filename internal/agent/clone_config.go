package agent

type agentCloneConfig struct {
	Command   string
	Model     string
	Reasoning ReasoningLevel
	Agentic   bool
	SessionID string
}

type agentCloneOption func(*agentCloneConfig)

func newAgentCloneConfig(
	command, model string,
	reasoning ReasoningLevel,
	agentic bool,
	sessionID string,
	opts ...agentCloneOption,
) agentCloneConfig {
	cfg := agentCloneConfig{
		Command:   command,
		Model:     model,
		Reasoning: reasoning,
		Agentic:   agentic,
		SessionID: sessionID,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func withClonedReasoning(level ReasoningLevel) agentCloneOption {
	return func(cfg *agentCloneConfig) {
		cfg.Reasoning = level
	}
}

func withClonedAgentic(agentic bool) agentCloneOption {
	return func(cfg *agentCloneConfig) {
		cfg.Agentic = agentic
	}
}

func withClonedModel(model string) agentCloneOption {
	return func(cfg *agentCloneConfig) {
		cfg.Model = model
	}
}

func withClonedSessionID(sessionID string) agentCloneOption {
	return func(cfg *agentCloneConfig) {
		cfg.SessionID = sessionID
	}
}
