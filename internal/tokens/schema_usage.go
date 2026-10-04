package tokens

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"io"
)

// ParseSchemaUsage reads usage emitted by sessionless Pi and Claude schema
// invocations. Non-JSON progress lines and unrelated events are ignored.
// Missing cost is distinct from an explicit zero-dollar run.
func ParseSchemaUsage(agentName string, r io.Reader) (*Usage, error) {
	if agentName != "pi" && agentName != "claude-code" {
		return nil, nil
	}
	reader := bufio.NewReader(r)
	var usage Usage
	var threadID string
	var missingPiCost bool
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
			TotalCostUSD *float64 `json:"total_cost_usd"`
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
					usage = Usage{
						InputTokens:         event.Usage.Input,
						OutputTokens:        event.Usage.Output,
						CachedInputTokens:   event.Usage.CacheRead,
						CacheCreationTokens: event.Usage.CacheCreation,
						UsageSource:         "job_log_claude_schema",
					}
					if event.TotalCostUSD != nil {
						usage.CostUSD = *event.TotalCostUSD
						usage.HasCost = true
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
