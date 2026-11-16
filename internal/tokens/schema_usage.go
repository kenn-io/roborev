package tokens

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"sort"
)

// claudeModelUsage is one model's entry in a Claude result event's modelUsage
// or model_usage_since_last_result object.
type claudeModelUsage struct {
	Input         int64    `json:"inputTokens"`
	Output        int64    `json:"outputTokens"`
	CacheRead     int64    `json:"cacheReadInputTokens"`
	CacheCreation int64    `json:"cacheCreationInputTokens"`
	CostUSD       *float64 `json:"costUSD"`
}

// claudeSessionUsage is the usage one Claude session has reported so far.
type claudeSessionUsage struct {
	usage Usage
	// unpriced records a counted result that carried no cost.
	unpriced bool
}

// sumClaudeModels totals per-model counts. The cost is reported only when
// every model carries one.
func sumClaudeModels(models map[string]claudeModelUsage) (Usage, bool) {
	var total Usage
	priced := true
	for _, model := range models {
		total.InputTokens += model.Input
		total.OutputTokens += model.Output
		total.CachedInputTokens += model.CacheRead
		total.CacheCreationTokens += model.CacheCreation
		if model.CostUSD == nil {
			priced = false
			continue
		}
		total.CostUSD += *model.CostUSD
	}
	return total, priced
}

// ParseSchemaUsage reads usage emitted by Pi and Claude invocations.
// Non-JSON progress lines and unrelated events are ignored. Missing cost is
// distinct from an explicit zero-dollar run.
//
// A Claude log can hold several result events: one session can report more
// than once, and a wrapper command can run the CLI several times in one job.
// Each result is counted once. model_usage_since_last_result is the usage since
// the session's previous result, so those are summed. A result without it
// reports the session's running total, in modelUsage or the top-level usage
// object, and replaces what that session reported before. Sessions are then
// summed. The per-model objects are preferred over the top-level usage object
// because they also count requests the CLI makes outside the main turn.
func ParseSchemaUsage(agentName string, r io.Reader) (*Usage, error) {
	if agentName != "pi" && agentName != "claude-code" {
		return nil, nil
	}
	reader := bufio.NewReader(r)
	var usage Usage
	var threadID string
	var missingPiCost bool
	claudeSessions := make(map[string]*claudeSessionUsage)
	var claudeSessionOrder []string
	for {
		line, err := reader.ReadBytes('\n')
		var event struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
			Message   struct {
				Role  string `json:"role"`
				Usage struct {
					Input      int64 `json:"input"`
					Output     int64 `json:"output"`
					CacheRead  int64 `json:"cacheRead"`
					CacheWrite int64 `json:"cacheWrite"`
					Cost       struct {
						Total *float64 `json:"total"`
					} `json:"cost"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				Input         int64 `json:"input_tokens"`
				Output        int64 `json:"output_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			TotalCostUSD        *float64                    `json:"total_cost_usd"`
			ModelUsage          map[string]claudeModelUsage `json:"modelUsage"`
			ModelUsageSinceLast map[string]claudeModelUsage `json:"model_usage_since_last_result"`
		}
		if json.Unmarshal(line, &event) == nil {
			switch agentName {
			case "pi":
				if event.Type == "session" {
					threadID = event.ID
				}
				if event.Type == "message_end" && event.Message.Role == "assistant" {
					counts := event.Message.Usage
					usage.InputTokens += counts.Input
					usage.OutputTokens += counts.Output
					usage.CachedInputTokens += counts.CacheRead
					usage.CacheCreationTokens += counts.CacheWrite
					usage.UsageSource = "job_log_pi_schema"
					if counts.Cost.Total == nil {
						missingPiCost = true
					} else {
						usage.CostUSD += *counts.Cost.Total
						usage.HasCost = true
					}
				}
			case "claude-code":
				if event.SessionID != "" {
					threadID = event.SessionID
				}
				if event.Type == "result" {
					// A result that omits its session belongs to the session
					// the stream last named.
					sessionID := threadID
					session := claudeSessions[sessionID]
					if session == nil {
						session = &claudeSessionUsage{}
						claudeSessions[sessionID] = session
						claudeSessionOrder = append(claudeSessionOrder, sessionID)
					}
					switch {
					case len(event.ModelUsageSinceLast) > 0:
						delta, priced := sumClaudeModels(event.ModelUsageSinceLast)
						session.usage.InputTokens += delta.InputTokens
						session.usage.OutputTokens += delta.OutputTokens
						session.usage.CachedInputTokens += delta.CachedInputTokens
						session.usage.CacheCreationTokens += delta.CacheCreationTokens
						session.usage.CostUSD += delta.CostUSD
						session.unpriced = session.unpriced || !priced
					case len(event.ModelUsage) > 0:
						total, priced := sumClaudeModels(event.ModelUsage)
						session.usage, session.unpriced = total, !priced
					default:
						session.usage = Usage{
							InputTokens:         event.Usage.Input,
							OutputTokens:        event.Usage.Output,
							CachedInputTokens:   event.Usage.CacheRead,
							CacheCreationTokens: event.Usage.CacheCreation,
						}
						session.unpriced = true
					}
					// total_cost_usd is the CLI's own running total for the
					// session, so it prices a result that reports a total.
					if len(event.ModelUsageSinceLast) == 0 && event.TotalCostUSD != nil {
						session.usage.CostUSD, session.unpriced = *event.TotalCostUSD, false
					}
				}
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(claudeSessionOrder) > 0 {
			usage = Usage{UsageSource: "job_log_claude_schema", HasCost: true}
			for _, sessionID := range claudeSessionOrder {
				session := claudeSessions[sessionID]
				usage.InputTokens += session.usage.InputTokens
				usage.OutputTokens += session.usage.OutputTokens
				usage.CachedInputTokens += session.usage.CachedInputTokens
				usage.CacheCreationTokens += session.usage.CacheCreationTokens
				usage.CostUSD += session.usage.CostUSD
				if session.unpriced {
					usage.HasCost = false
				}
				if sessionID != "" {
					usage.ProviderSessionIDs = append(usage.ProviderSessionIDs, sessionID)
				}
			}
			// Name the sessions only when several ran. A single session is
			// already identified by ThreadID.
			if len(claudeSessionOrder) > 1 {
				sort.Strings(usage.ProviderSessionIDs)
				usage.ExpectedProviderSessions = len(claudeSessionOrder)
			} else {
				usage.ProviderSessionIDs = nil
			}
			if !usage.HasCost {
				usage.CostUSD = 0
			}
		}
		usage.ThreadID = threadID
		if missingPiCost {
			usage.HasCost = false
		}
		if !usage.HasUsageData() {
			return nil, nil
		}
		return &usage, nil
	}
}

// ParseJobLogUsageFile extracts token usage from a raw job log, reading the
// event format of the agent that wrote it. A missing log is not an error.
func ParseJobLogUsageFile(agentName, path string) (*Usage, error) {
	if agentName != "claude-code" {
		return ParseCodexUsageFile(path)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return ParseSchemaUsage(agentName, f)
}
