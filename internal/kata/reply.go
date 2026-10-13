package kata

import (
	"context"
	"fmt"
	"strings"
)

// ReplyReq links an assessment to an existing Kata finding comment.
type ReplyReq struct {
	Issue          string
	Target         string
	Kind           string
	Body           string
	IdempotencyKey string
}

// Reply records a typed assertion. It requires Kata's comment-relation support.
func (c *CLIClient) Reply(ctx context.Context, req ReplyReq) error {
	flag := ""
	switch req.Kind {
	case "confirmed":
		flag = "--confirm"
	case "refuted":
		flag = "--refute"
	case "superseded":
		flag = "--supersede"
	default:
		return fmt.Errorf("unsupported Kata reply kind %q", req.Kind)
	}
	if strings.TrimSpace(req.Issue) == "" || strings.TrimSpace(req.Target) == "" || strings.TrimSpace(req.Body) == "" {
		return fmt.Errorf("kata reply requires issue, target and body")
	}
	if strings.HasPrefix(req.Issue, "-") || strings.HasPrefix(req.Target, "-") {
		return fmt.Errorf("invalid kata reply reference")
	}
	args := []string{"comment", req.Issue, flag, req.Target, "--body-stdin", "--json"}
	if req.IdempotencyKey != "" {
		args = append(args, "--idempotency-key", req.IdempotencyKey)
	}
	_, err := c.run(ctx, c, args, strings.NewReader(req.Body))
	return err
}
