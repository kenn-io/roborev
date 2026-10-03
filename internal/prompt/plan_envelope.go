package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
)

const fixPlanMarker = "roborev-fix-plan-v1\n"

const invalidFixPlanDisplayMessage = "Fix plan prompt is unavailable because its stored envelope is invalid."

type fixPlanEnvelope struct {
	PlanningPrompt       string `json:"planning_prompt"`
	ImplementationPrompt string `json:"implementation_prompt"`
}

// EncodeFixPlan preserves both phases in the existing durable job prompt.
func EncodeFixPlan(planningPrompt, implementationPrompt string) string {
	encoded, _ := json.Marshal(fixPlanEnvelope{planningPrompt, implementationPrompt})
	return fixPlanMarker + string(encoded)
}

// DecodeFixPlan distinguishes legacy prompts from recognized phase envelopes.
// A corrupt or unsupported envelope must never run as an implementation prompt.
func DecodeFixPlan(stored string) (planningPrompt, implementationPrompt string, planned bool, err error) {
	if !strings.HasPrefix(stored, "roborev-fix-plan-") {
		return "", stored, false, nil
	}
	if !strings.HasPrefix(stored, fixPlanMarker) {
		return "", "", true, fmt.Errorf("unsupported fix plan envelope version")
	}
	var envelope fixPlanEnvelope
	if err := json.Unmarshal([]byte(strings.TrimPrefix(stored, fixPlanMarker)), &envelope); err != nil {
		return "", "", true, fmt.Errorf("decode fix plan envelope: %w", err)
	}
	if strings.TrimSpace(envelope.PlanningPrompt) == "" || strings.TrimSpace(envelope.ImplementationPrompt) == "" {
		return "", "", true, fmt.Errorf("fix plan envelope requires planning and implementation prompts")
	}
	return envelope.PlanningPrompt, envelope.ImplementationPrompt, true, nil
}

// DisplayFixPlanPrompt returns a readable view of a stored fix prompt while
// keeping the durable phase envelope out of user-facing job responses.
func DisplayFixPlanPrompt(stored string) string {
	planningPrompt, implementationPrompt, planned, err := DecodeFixPlan(stored)
	if !planned {
		return implementationPrompt
	}
	if err != nil {
		return invalidFixPlanDisplayMessage
	}
	return "## Planning Prompt\n\n" + planningPrompt + "\n\n## Implementation Prompt\n\n" + implementationPrompt
}
