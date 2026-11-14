package agentwire

// GeminiDeprecatedPromptMessage is the CLI's stream-json warning for --prompt.
const GeminiDeprecatedPromptMessage = "The --prompt (-p) flag has been deprecated and will be removed in a future version. Please use a positional argument for your prompt. See gemini --help for more information."

// AntigravityResult is the documented terminal result in Antigravity output.
type AntigravityResult struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Error    string `json:"error"`
}
