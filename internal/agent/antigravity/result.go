package antigravity

// Result is the documented terminal result in Antigravity's stream output.
type Result struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Error    string `json:"error"`
}
