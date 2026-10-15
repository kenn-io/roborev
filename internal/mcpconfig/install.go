// Package mcpconfig installs the roborev MCP entry in coding-agent configuration.
package mcpconfig

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	"go.kenn.io/roborev/internal/agentconfig"
	"go.kenn.io/roborev/internal/skills"
)

type Options struct {
	ConfigDir  string
	Agent      skills.Agent
	ConfigPath string
	Executable string
	Server     string
	URL        string
	Transport  string
	DryRun     bool
}

type Result struct {
	Path    string
	Data    []byte
	Changed bool
}

// Install merges one server entry. An explicit URL selects the existing daemon's
// HTTP endpoint; stdio runs the installed binary and uses its daemon discovery.
func Install(opts Options) (Result, error) {
	if err := Validate(opts); err != nil {
		return Result{}, err
	}
	name, err := ConfigPath(opts)
	if err != nil {
		return Result{}, err
	}
	original, err := os.ReadFile(name)
	if err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	data, err := Merge(original, opts)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", name, err)
	}
	result := Result{Path: name, Data: data, Changed: !bytes.Equal(original, data)}
	if !opts.DryRun && result.Changed {
		if err := agentconfig.Write(name, data); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

// ConfigPath resolves the destination without reading or parsing configuration.
func ConfigPath(opts Options) (string, error) {
	dir, err := skills.ConfigDir(opts.Agent)
	if err != nil {
		return "", err
	}
	if opts.ConfigDir != "" {
		dir = opts.ConfigDir
	}
	name := ""
	switch opts.Agent {
	case skills.AgentClaude:
		name = ".claude.json"
		if opts.ConfigDir != "" || os.Getenv("CLAUDE_CONFIG_DIR") != "" {
			name = filepath.Join(dir, ".claude.json")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			name = filepath.Join(home, name)
		}
	case skills.AgentCodex, skills.AgentGrok:
		name = "config.toml"
	case skills.AgentDroid, skills.AgentCursor:
		name = "mcp.json"
	case skills.AgentCopilot:
		name = "mcp-config.json"
	case skills.AgentGemini, skills.AgentQwen:
		name = "settings.json"
	case skills.AgentHermes:
		name = "config.yaml"
	case skills.AgentZcode:
		name = filepath.Join("cli", "config.json")
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}
	if opts.ConfigPath != "" {
		name = opts.ConfigPath
	}
	return name, nil
}

// Merge adds the MCP entry to configuration bytes without accessing the filesystem.
func Merge(data []byte, opts Options) ([]byte, error) {
	if opts.Transport == "" {
		opts.Transport = "stdio"
	}
	if err := Validate(opts); err != nil {
		return nil, err
	}
	key, format := "mcpServers", "json"
	switch opts.Agent {
	case skills.AgentCodex, skills.AgentGrok:
		key, format = "mcp_servers", "toml"
	case skills.AgentHermes:
		key, format = "mcp_servers", "yaml"
	case skills.AgentZcode:
		// ZCode nests server entries under mcp.servers in its shared
		// configuration file; a dotted key addresses the nested object.
		key, format = "mcp.servers", "json"
	}
	var err error
	doc := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		switch format {
		case "toml":
			err = toml.Unmarshal(data, &doc)
		case "yaml":
			err = yaml.Unmarshal(data, &doc)
		default:
			err = json.Unmarshal(data, &doc)
		}
		if err != nil {
			return nil, fmt.Errorf("parse MCP config: %w", err)
		}
	}
	if doc == nil {
		return nil, fmt.Errorf("MCP config must contain a configuration object")
	}
	servers, err := serversObject(doc, key)
	if err != nil {
		return nil, err
	}
	entry := map[string]any{}
	if opts.Transport == "stdio" {
		binary := opts.Executable
		if binary == "" {
			binary = "roborev"
		}
		args := []string{"mcp", "serve"}
		if opts.Server != "" {
			args = append([]string{"--server", opts.Server}, args...)
		}
		entry["command"], entry["args"] = binary, args
	} else {
		urlKey := "url"
		if opts.Agent == skills.AgentGemini || opts.Agent == skills.AgentQwen {
			urlKey = "httpUrl"
		}
		entry[urlKey] = opts.URL
	}
	switch opts.Agent {
	case skills.AgentClaude, skills.AgentDroid, skills.AgentZcode:
		entry["type"] = opts.Transport
	case skills.AgentCopilot:
		entry["type"] = opts.Transport
		if opts.Transport == "stdio" {
			entry["type"] = "local"
		}
		entry["tools"] = []string{"*"}
	}
	servers["roborev"] = entry
	var buf bytes.Buffer
	switch format {
	case "toml":
		err = toml.NewEncoder(&buf).Encode(doc)
	case "yaml":
		err = yaml.NewEncoder(&buf).Encode(doc)
	default:
		err = json.MarshalWrite(&buf, doc, jsontext.WithIndent("  "), json.Deterministic(true))
		buf.WriteByte('\n')
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// serversObject resolves the server map at key, creating missing parent
// objects. A dotted key addresses a nested object such as ZCode's mcp.servers.
func serversObject(doc map[string]any, key string) (map[string]any, error) {
	parts := strings.Split(key, ".")
	parent := doc
	for _, part := range parts[:len(parts)-1] {
		raw, ok := parent[part]
		if !ok || raw == nil {
			child := map[string]any{}
			parent[part] = child
			parent = child
			continue
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", strings.Join(parts, "."))
		}
		parent = object
	}
	leaf := parts[len(parts)-1]
	raw, ok := parent[leaf]
	if !ok || raw == nil {
		servers := map[string]any{}
		parent[leaf] = servers
		return servers, nil
	}
	servers, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return servers, nil
}

// DaemonAddress resolves the API base URL belonging to an existing MCP endpoint.
func DaemonAddress(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "/mcp" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("HTTP transport requires an absolute daemon URL ending in /mcp")
	}
	u.Path = ""
	return u.String(), nil
}

// Validate checks transport arguments before configuration discovery.
func Validate(opts Options) error {
	if opts.Transport == "" {
		opts.Transport = "stdio"
	}
	if opts.Transport != "stdio" && opts.Transport != "http" {
		return fmt.Errorf("transport must be stdio or http")
	}
	if opts.Transport == "http" {
		if _, err := DaemonAddress(opts.URL); err != nil {
			return err
		}
	}
	if opts.Transport == "stdio" && opts.URL != "" {
		return fmt.Errorf("--url requires --transport http")
	}
	return nil
}
