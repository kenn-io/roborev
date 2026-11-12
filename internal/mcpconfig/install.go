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
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	"github.com/creachadair/tomledit/transform"
	"github.com/tailscale/hujson"
	"go.kenn.io/kit/agentmcp"
	kitdaemon "go.kenn.io/kit/daemon"
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
	if opts.Agent == skills.AgentClaude || opts.Agent == skills.AgentCodex {
		result, err := agentmcp.PlanInstall(agentmcp.Agent(opts.Agent), agentmcp.InstallOptions{ConfigPath: name, Name: "roborev", Server: Server(opts)})
		if err != nil {
			return Result{}, err
		}
		if !opts.DryRun && result.Changed {
			if err := agentconfig.Write(name, result.Data); err != nil {
				return Result{}, err
			}
		}
		return Result{Path: result.ConfigPath, Data: result.Data, Changed: result.Changed}, nil
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

// Server translates RoboRev's MCP options to Kit's server descriptor.
func Server(opts Options) agentmcp.Server {
	if opts.Transport == "http" {
		return agentmcp.Server{Transport: agentmcp.TransportHTTP, URL: opts.URL}
	}
	binary := opts.Executable
	if binary == "" {
		binary = "roborev"
	}
	args := []string{"mcp", "serve"}
	if opts.Server != "" {
		args = append([]string{"--server", opts.Server}, args...)
	}
	return agentmcp.Server{Command: binary, Args: args}
}

// RegisteredServer is one installed Claude or Codex connection, including
// Claude's headersHelper, which Kit's server type cannot represent.
type RegisteredServer struct {
	Transport         agentmcp.Transport `json:"type"`
	Command           string             `json:"command"`
	Args              []string           `json:"args"`
	Env               map[string]string  `json:"env"`
	URL               string             `json:"url"`
	Headers           map[string]string  `json:"headers" toml:"http_headers"`
	BearerTokenEnvVar string             `json:"bearer_token_env_var" toml:"bearer_token_env_var"`
	HeadersHelper     string             `json:"headersHelper" toml:"-"`
}

// Server returns the connection fields Kit can write and check.
func (r RegisteredServer) Server() agentmcp.Server {
	return agentmcp.Server{Transport: r.Transport, Command: r.Command, Args: r.Args, Env: r.Env, URL: r.URL, Headers: r.Headers, BearerTokenEnvVar: r.BearerTokenEnvVar}
}

// Registration reads the installed Claude or Codex server. An absent or
// unreadable entry is not registered.
func Registration(opts Options) (RegisteredServer, bool) {
	path, err := ConfigPath(opts)
	if err != nil {
		return RegisteredServer{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RegisteredServer{}, false
	}
	var server RegisteredServer
	if opts.Agent == skills.AgentCodex {
		var doc struct {
			Servers map[string]RegisteredServer `toml:"mcp_servers"`
		}
		err = toml.Unmarshal(data, &doc)
		server = doc.Servers["roborev"]
	} else if data, err = hujson.Standardize(data); err == nil {
		var doc struct {
			Servers map[string]jsontext.Value `json:"mcpServers"`
		}
		if err = json.Unmarshal(data, &doc); err == nil && len(doc.Servers["roborev"]) > 0 {
			err = json.Unmarshal(doc.Servers["roborev"], &server)
		}
	}
	if err != nil || server.Command == "" && server.URL == "" {
		return RegisteredServer{}, false
	}
	return server, true
}

// DaemonAddress returns the daemon selected by the registration, if any.
func (r RegisteredServer) DaemonAddress() string {
	if r.URL != "" {
		u, err := url.Parse(r.URL)
		if err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
		return ""
	}
	address := ""
	for i := 0; i < len(r.Args); i++ {
		if r.Args[i] == "--server" && i+1 < len(r.Args) {
			i++
			address = r.Args[i]
		} else if value, ok := strings.CutPrefix(r.Args[i], "--server="); ok {
			address = value
		}
	}
	return address
}

// WithServer points the connection at a daemon address and keeps its
// transport and other settings.
func (r RegisteredServer) WithServer(address string) (RegisteredServer, error) {
	if r.URL != "" {
		host := ""
		if strings.HasPrefix(address, "https://") {
			endpoint, err := url.Parse(address)
			if err == nil {
				host = endpoint.Host
			}
		} else if endpoint, err := kitdaemon.ParseEndpoint(address, kitdaemon.ParseEndpointOptions{}); err == nil && !endpoint.IsUnix() {
			host = endpoint.Address
		}
		if host == "" {
			return r, fmt.Errorf("HTTP MCP transport requires an HTTP daemon address")
		}
		current, err := url.Parse(r.URL)
		if err != nil {
			return r, err
		}
		current.Host = host
		r.URL = current.String()
	} else {
		args := []string{"--server", address}
		for i := 0; i < len(r.Args); i++ {
			if r.Args[i] == "--server" {
				i++
			} else if !strings.HasPrefix(r.Args[i], "--server=") {
				args = append(args, r.Args[i])
			}
		}
		r.Args = args
	}
	return r, nil
}

func patchJSON(data []byte, pointer string, value any) ([]byte, error) {
	root, err := hujson.Parse(data)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := root.Patch(fmt.Appendf(nil, `[{"op":"add","path":%q,"value":%s}]`, pointer, encoded)); err != nil {
		return nil, err
	}
	return root.Pack(), nil
}

// Repair writes the selected server's daemon field into the existing entry,
// re-enables a disabled Codex entry. Every other field and comment stays as
// the user wrote it, which Kit's
// writer would replace.
func Repair(opts Options, server RegisteredServer) error {
	path, err := ConfigPath(opts)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	current, _ := Registration(opts)
	changed := false
	fields := map[string]any{}
	if server.URL != current.URL {
		fields["url"] = server.URL
	}
	if !slices.Equal(server.Args, current.Args) {
		fields["args"] = server.Args
	}
	repaired := data
	if opts.Agent == skills.AgentCodex {
		doc, err := tomledit.Parse(bytes.NewReader(data))
		if err != nil {
			return err
		}
		for field, value := range fields {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			parsed, err := parser.ParseValue(string(encoded))
			if err != nil {
				return err
			}
			if entry := doc.First("mcp_servers", "roborev", field); entry != nil {
				entry.Value.X = parsed.X
				changed = true
			} else {
				mapping := &parser.KeyValue{Name: parser.Key{field}, Value: parsed}
				if entry := doc.First("mcp_servers", "roborev"); entry != nil && entry.KeyValue != nil {
					inline, ok := entry.Value.X.(parser.Inline)
					if !ok {
						return fmt.Errorf("cannot update MCP registration's %s field", field)
					}
					entry.Value.X = append(inline, mapping)
				} else if entry := doc.First("mcp_servers", "roborev", "command"); entry != nil {
					mapping.Name = append(slices.Clone(entry.Name[:len(entry.Name)-1]), field)
					transform.InsertMapping(entry.Section, mapping, false)
				} else {
					return fmt.Errorf("cannot update MCP registration's %s field", field)
				}
				changed = true
			}
		}
		for _, entry := range doc.Find("mcp_servers", "roborev", "enabled") {
			changed = entry.Value.X.String() == "false" && entry.Remove() || changed
		}
		if changed {
			var out bytes.Buffer
			if err := tomledit.Format(&out, doc); err != nil {
				return err
			}
			repaired = out.Bytes()
		}
	} else {
		for field, value := range fields {
			if repaired, err = patchJSON(repaired, "/mcpServers/roborev/"+field, value); err != nil {
				return err
			}
		}
	}
	if opts.DryRun || bytes.Equal(repaired, data) {
		return nil
	}
	return agentconfig.Write(path, repaired)
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
		server := Server(opts)
		entry["command"], entry["args"] = server.Command, server.Args
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
