package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// RemoteCapability is the tailnet app capability that grants remote API
// access. Its grant values carry {"access": "read"|"queue"}.
const RemoteCapability = "kenn.io/cap/roborev"

// RemoteAccess is the access level a tailnet policy grant gives a caller.
type RemoteAccess int

const (
	RemoteAccessNone RemoteAccess = iota
	RemoteAccessRead
	RemoteAccessQueue
)

func (a RemoteAccess) String() string {
	switch a {
	case RemoteAccessRead:
		return "read"
	case RemoteAccessQueue:
		return "queue"
	default:
		return "none"
	}
}

// RemoteCaller is a tailnet peer identified by tailscaled.
type RemoteCaller struct {
	Node   string
	Login  string
	Tags   []string
	Access RemoteAccess
}

type whoisFunc func(ctx context.Context, peer string) (RemoteCaller, error)

// tailscaleWhois identifies peers by running the tailscale CLI, which works
// with every tailscaled install and needs no Tailscale Go dependency.
func tailscaleWhois(binary string) whoisFunc {
	if binary == "" {
		binary = "tailscale"
	}
	return func(ctx context.Context, peer string) (RemoteCaller, error) {
		cmd := exec.CommandContext(ctx, binary, "whois", "--json", peer)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return RemoteCaller{}, fmt.Errorf("tailscale whois failed for %s: %w: %s",
				peer, err, strings.TrimSpace(stderr.String()))
		}
		return parseWhois(out)
	}
}

type whoisResponse struct {
	Node struct {
		Name string   `json:"Name"`
		Tags []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
	CapMap map[string][]json.RawMessage `json:"CapMap"`
}

func parseWhois(out []byte) (RemoteCaller, error) {
	var resp whoisResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return RemoteCaller{}, fmt.Errorf("decode tailscale whois output: %w", err)
	}
	caller := RemoteCaller{
		Node:  resp.Node.Name,
		Login: resp.UserProfile.LoginName,
		Tags:  resp.Node.Tags,
	}
	for _, raw := range resp.CapMap[RemoteCapability] {
		var grant struct {
			Access string `json:"access"`
		}
		// A grant entry this daemon does not understand grants nothing;
		// it must never widen access.
		if json.Unmarshal(raw, &grant) != nil {
			continue
		}
		switch grant.Access {
		case "queue":
			caller.Access = max(caller.Access, RemoteAccessQueue)
		case "read":
			caller.Access = max(caller.Access, RemoteAccessRead)
		}
	}
	return caller, nil
}
