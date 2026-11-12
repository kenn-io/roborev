package kata

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// DocbankHealth is the credential state Kata exposes in its health response.
// An empty credential means the daemon did not expose that state.
type DocbankHealth struct {
	Credential string `json:"credential"`
}

// DocbankHealth reads Kata health without starting its daemon.
func (c *CLIClient) DocbankHealth(ctx context.Context) (DocbankHealth, error) {
	out, err := c.run(ctx, c, []string{"health", "--json"}, nil)
	if err != nil {
		return DocbankHealth{}, err
	}
	var response struct {
		Docbank *DocbankHealth `json:"docbank"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return DocbankHealth{}, fmt.Errorf("parse kata health: %w", err)
	}
	if response.Docbank == nil {
		return DocbankHealth{}, nil
	}
	return *response.Docbank, nil
}
