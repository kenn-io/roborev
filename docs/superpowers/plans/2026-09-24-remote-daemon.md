# Remote Daemon Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a roborev client on one tailnet machine read reviews from and
queue reviews on a daemon on another machine, authenticated by Tailscale
identity.

**Architecture:** The daemon gains an opt-in second HTTP listener on its
Tailscale address. A remote handler identifies each connection with
`tailscale whois`, enforces a `read`/`queue` allowlist, maps repo identities to
daemon checkouts, and forwards to the existing core mux. Unpushed commits reach
the daemon as a git pack. The CLI gains a remote mode, selected by
`[remote] server` or a non-loopback `--server http://host:port`. In remote mode
it never manages a local daemon, sends identities instead of local paths, and
uploads packs when the daemon reports missing commits.

**Tech Stack:** Go standard library, huma (existing), `go.kenn.io/kit/git/cmd`
(existing), the `tailscale` CLI at runtime.

**Spec:** `docs/superpowers/specs/2026-09-24-remote-daemon-design.md`

## Global Constraints

- No new Go module dependencies. Tailscale is reached only through its CLI.
- The loopback API, Unix socket, and `kitdaemon.RequireLoopback` stay
  unchanged. No change to `kenn-io/kit`.
- Remote capability name: `kenn.io/cap/roborev`. Access values: `read`,
  `queue`.
- `remote_api.listen` must be a literal address in `100.64.0.0/10` or
  `fd7a:115c:a1e0::/48` with a non-zero port.
- The remote `http.Server` uses `ReadHeaderTimeout: 5 * time.Second` and
  `IdleTimeout: 2 * time.Minute` (the browser listener's values), and no
  `ReadTimeout` or `WriteTimeout`.
- Upload refs live under `refs/roborev/uploads/<sha>`.
- No size limit on pack uploads.
- Tests use testify (`require` for preconditions, `assert` otherwise;
  `assert := assert.New(t)` past three assertions). No `t.Fatal`/`t.Error`,
  and no `assert.True(t, a == b)`.
- Test packages that run git must keep their `TestMain` calling
  `testenv.RunIsolatedMain` (both `internal/daemon` and `cmd/roborev`
  already do).
- After Go changes run `go fmt ./...` and `go vet ./...`. Put the pinned
  golangci-lint 2.13.1 first on `PATH` for commits.
- Never run `make install` or install a `roborev` binary into PATH.
- No emojis in code or output.

## Review Focus

- **Unknown grant values:** a grant of `{"access": "admin"}`, or a malformed
  entry, gives no access (`403`), never read access. Test in Task 3.
- **Both `repo_path` and `repo_identity`:** a remote enqueue that sends both
  gets `400`, and the daemon never uses the path. Test in Task 5.
- **Annotated tags and non-commit refs in the daemon clone:** `have` holds only
  peeled commit SHAs, so the client's `pack-objects` never sees a tree or tag
  exclusion. Test in Task 4.
- **Checkout with no remote and no `.roborev-id`:** a remote review fails
  before any request, with a message telling the user to add `.roborev-id`.
  Test in Task 8.
- **Branch names that git rejects** (for example `feat..x` or `-x`): `400`,
  not a `500` or a stored job. Test in Task 5.

---

### Task 1: Config sections for the remote listener and remote client

**Files:**
- Modify: `internal/config/config.go` (`Config` struct near `Web WebConfig`
  at ~366, `normalizeGlobalConfig` at ~982)
- Test: `internal/config/remote_config_test.go`

**Interfaces:**
- Produces: `config.RemoteAPIConfig{Enabled bool; Listen string; TailscalePath string}`,
  `config.RemoteConfig{Server string}`, fields `Config.RemoteAPI` (`toml:"remote_api"`)
  and `Config.Remote` (`toml:"remote"`),
  `func IsTailscaleAddr(addr netip.Addr) bool`, and
  `func LoadRemoteServer() (string, error)`.

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteAPIConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "disabled ignores listen", body: "[remote_api]\nlisten = \"0.0.0.0:1\"\n"},
		{name: "tailscale ipv4", body: "[remote_api]\nenabled = true\nlisten = \"100.101.102.103:7474\"\n"},
		{name: "tailscale ipv6", body: "[remote_api]\nenabled = true\nlisten = \"[fd7a:115c:a1e0::1]:7474\"\n"},
		{name: "lan address", body: "[remote_api]\nenabled = true\nlisten = \"192.168.1.5:7474\"\n", wantErr: "not a Tailscale address"},
		{name: "unspecified", body: "[remote_api]\nenabled = true\nlisten = \"0.0.0.0:7474\"\n", wantErr: "not a Tailscale address"},
		{name: "hostname", body: "[remote_api]\nenabled = true\nlisten = \"host.example:7474\"\n", wantErr: "must be a Tailscale IP and port"},
		{name: "zero port", body: "[remote_api]\nenabled = true\nlisten = \"100.101.102.103:0\"\n", wantErr: "fixed port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadGlobalFrom(writeGlobalConfig(t, tt.body))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cfg)
		})
	}
}

func TestRemoteClientConfigParses(t *testing.T) {
	cfg, err := LoadGlobalFrom(writeGlobalConfig(t,
		"[remote]\nserver = \"http://daemon-host.example:7474\"\n"))
	require.NoError(t, err)
	assert.Equal(t, "http://daemon-host.example:7474", cfg.Remote.Server)
}

func TestLoadRemoteServerIgnoresUnrelatedInvalidSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	server, err := LoadRemoteServer()
	require.NoError(t, err, "missing config file means no remote server")
	assert.Empty(t, server)

	// remote_api.listen is invalid, which full validation rejects, but
	// reading [remote] server must still work.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[remote_api]\nenabled = true\nlisten = \"0.0.0.0:1\"\n[remote]\nserver = \"http://daemon-host.example:7474\"\n"), 0o644))
	server, err = LoadRemoteServer()
	require.NoError(t, err)
	assert.Equal(t, "http://daemon-host.example:7474", server)
}

func TestIsTailscaleAddr(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsTailscaleAddr(netip.MustParseAddr("100.64.0.1")))
	assert.True(IsTailscaleAddr(netip.MustParseAddr("100.127.255.254")))
	assert.False(IsTailscaleAddr(netip.MustParseAddr("100.128.0.1")))
	assert.True(IsTailscaleAddr(netip.MustParseAddr("fd7a:115c:a1e0::5")))
	assert.True(IsTailscaleAddr(netip.MustParseAddr("::ffff:100.64.0.1")))
	assert.False(IsTailscaleAddr(netip.MustParseAddr("10.0.0.1")))
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/config -run 'TestRemoteAPIConfigValidation|TestRemoteClientConfigParses|TestIsTailscaleAddr'`
Expected: compile failure (`IsTailscaleAddr` and `cfg.Remote` undefined).

- [ ] **Step 3: Implement**

Add next to `MCPConfig` in `config.go`:

```go
// RemoteAPIConfig configures the listener that serves the daemon API to
// other tailnet machines. Callers are identified by tailscaled.
type RemoteAPIConfig struct {
	Enabled       bool   `toml:"enabled" comment:"Serve the daemon API to tailnet peers granted kenn.io/cap/roborev."`
	Listen        string `toml:"listen" comment:"This host's Tailscale IP and port, for example 100.101.102.103:7474."`
	TailscalePath string `toml:"tailscale_path" comment:"Path to the tailscale CLI. Empty uses tailscale from PATH."`
}

// RemoteConfig points the CLI at a daemon on another machine.
type RemoteConfig struct {
	Server string `toml:"server" comment:"Remote daemon URL, for example http://daemon-host.example-tailnet.ts.net:7474."`
}

var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// IsTailscaleAddr reports whether addr is in Tailscale's address ranges.
func IsTailscaleAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	return tailscaleIPv4.Contains(addr) || tailscaleIPv6.Contains(addr)
}

// LoadRemoteServer reads only [remote] server from the global config. The
// CLI checks it before every command, so it must not fail on unrelated
// settings that full validation would reject. A missing file means no
// remote server.
func LoadRemoteServer() (string, error) {
	var cfg struct {
		Remote RemoteConfig `toml:"remote"`
	}
	if _, err := toml.DecodeFile(GlobalConfigPath(), &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read [remote] server from %s: %w", GlobalConfigPath(), err)
	}
	return strings.TrimSpace(cfg.Remote.Server), nil
}

func normalizeRemoteAPIConfig(remote *RemoteAPIConfig) error {
	if !remote.Enabled {
		return nil
	}
	addrPort, err := netip.ParseAddrPort(remote.Listen)
	if err != nil {
		return fmt.Errorf(
			"remote_api.listen %q must be a Tailscale IP and port, such as 100.101.102.103:7474: %w",
			remote.Listen, err)
	}
	if !IsTailscaleAddr(addrPort.Addr()) {
		return fmt.Errorf(
			"remote_api.listen %q is not a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48); only tailnet peers can be identified",
			remote.Listen)
	}
	if addrPort.Port() == 0 {
		return fmt.Errorf("remote_api.listen %q needs a fixed port", remote.Listen)
	}
	return nil
}
```

Add to `Config` beside `Web`:

```go
	RemoteAPI RemoteAPIConfig `toml:"remote_api"`
	Remote    RemoteConfig    `toml:"remote"`
```

Change the end of `normalizeGlobalConfig`:

```go
	if err := normalizeWebConfig(&cfg.Web); err != nil {
		return err
	}
	return normalizeRemoteAPIConfig(&cfg.RemoteAPI)
```

Add `"net/netip"`, `"io/fs"`, and (if not already imported) `"errors"` and
`"strings"` to the imports. `toml` is the package `LoadGlobalFrom` already
uses. Add `"os"` and `"path/filepath"` to the test imports.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/config`
Expected: PASS. `roborev config get remote_api.listen` works through the
reflection-based key lookup with no `keyval.go` change.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/remote_config_test.go
git commit -m "feat(config): add remote_api listener and remote client settings"
```

---

### Task 2: Find repos by identity without sync placeholders

**Files:**
- Modify: `internal/storage/sync.go` (after `GetRepoByIdentity` at ~268)
- Test: `internal/storage/repos_identity_test.go`

**Interfaces:**
- Produces: `func (db *DB) FindReposByIdentity(identity string) ([]Repo, error)`.
  Results are ordered by `root_path` and exclude rows where
  `root_path = identity`.

- [ ] **Step 1: Write the failing test**

```go
package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindReposByIdentity(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	const id = "https://example.com/org/project.git"

	_, err := db.GetOrCreateRepoByIdentity(id) // sync placeholder: root_path == identity
	require.NoError(t, err)
	none, err := db.FindReposByIdentity(id)
	require.NoError(t, err)
	assert.Empty(none)

	_, err = db.GetOrCreateRepo("/srv/b/project", id)
	require.NoError(t, err)
	_, err = db.GetOrCreateRepo("/srv/a/project", id)
	require.NoError(t, err)
	_, err = db.GetOrCreateRepo("/srv/other", "https://example.com/org/other.git")
	require.NoError(t, err)

	repos, err := db.FindReposByIdentity(id)
	require.NoError(t, err)
	require.Len(t, repos, 2)
	assert.Equal("/srv/a/project", repos[0].RootPath)
	assert.Equal("/srv/b/project", repos[1].RootPath)
	assert.Equal(id, repos[0].Identity)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/storage -run TestFindReposByIdentity`
Expected: compile failure (`FindReposByIdentity` undefined).

- [ ] **Step 3: Implement**

```go
// FindReposByIdentity returns registered repos with a real checkout whose
// identity matches exactly. Sync placeholders (root_path == identity) are
// excluded because they have no checkout to review in.
func (db *DB) FindReposByIdentity(identity string) ([]Repo, error) {
	rows, err := db.Query(`
		SELECT id, root_path, name, created_at, identity
		FROM repos
		WHERE identity = ? AND root_path != identity
		ORDER BY root_path
	`, identity)
	if err != nil {
		return nil, fmt.Errorf("query repos by identity: %w", err)
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		var r Repo
		var createdAt string
		var identityVal sql.NullString
		if err := rows.Scan(&r.ID, &r.RootPath, &r.Name, &createdAt, &identityVal); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		r.Identity = identityVal.String
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find repos by identity: %w", err)
	}
	return repos, nil
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/storage -run 'TestFindReposByIdentity|TestGetRepoByIdentity'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/sync.go internal/storage/repos_identity_test.go
git commit -m "feat(storage): find checkout repos by identity"
```

---

### Task 3: Identify callers with `tailscale whois`

**Files:**
- Create: `internal/daemon/remote_whois.go`
- Test: `internal/daemon/remote_whois_test.go`

**Interfaces:**
- Produces:
  - `const RemoteCapability = "kenn.io/cap/roborev"`
  - `type RemoteAccess int` with `RemoteAccessNone`, `RemoteAccessRead`,
    `RemoteAccessQueue`, and a `String()` method returning `none`, `read`, or
    `queue`
  - `type RemoteCaller struct{ Node, Login string; Tags []string; Access RemoteAccess }`
  - `type whoisFunc func(ctx context.Context, peer string) (RemoteCaller, error)`
  - `func tailscaleWhois(binary string) whoisFunc`
  - `func parseWhois(out []byte) (RemoteCaller, error)`

- [ ] **Step 1: Write the failing tests**

```go
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func whoisJSON(capEntries string) string {
	return `{"Node":{"Name":"laptop.example-tailnet.ts.net.","Tags":["tag:dev"]},` +
		`"UserProfile":{"LoginName":"user-a@example.com"},` +
		`"CapMap":{"kenn.io/cap/roborev":[` + capEntries + `]}}`
}

func TestParseWhoisAccessLevels(t *testing.T) {
	tests := []struct {
		name    string
		entries string
		want    RemoteAccess
	}{
		{"read", `{"access":"read"}`, RemoteAccessRead},
		{"queue", `{"access":"queue"}`, RemoteAccessQueue},
		{"highest wins", `{"access":"read"},{"access":"queue"}`, RemoteAccessQueue},
		{"unknown value", `{"access":"admin"}`, RemoteAccessNone},
		{"malformed entry", `"queue"`, RemoteAccessNone},
		{"no entries", ``, RemoteAccessNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller, err := parseWhois([]byte(whoisJSON(tt.entries)))
			require.NoError(t, err)
			assert.Equal(t, tt.want, caller.Access)
		})
	}
}

func TestParseWhoisIdentity(t *testing.T) {
	assert := assert.New(t)
	caller, err := parseWhois([]byte(whoisJSON(`{"access":"read"}`)))
	require.NoError(t, err)
	assert.Equal("laptop.example-tailnet.ts.net.", caller.Node)
	assert.Equal("user-a@example.com", caller.Login)
	assert.Equal([]string{"tag:dev"}, caller.Tags)
}

func TestParseWhoisRejectsGarbage(t *testing.T) {
	_, err := parseWhois([]byte("not json"))
	require.ErrorContains(t, err, "decode tailscale whois output")
}

func TestTailscaleWhoisRunsCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tailscale CLI is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "tailscale")
	body := "#!/bin/sh\n[ \"$1 $2 $3\" = \"whois --json 100.64.0.2:5555\" ] || { echo bad args >&2; exit 2; }\n" +
		"cat <<'EOF'\n" + whoisJSON(`{"access":"queue"}`) + "\nEOF\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))

	caller, err := tailscaleWhois(script)(context.Background(), "100.64.0.2:5555")
	require.NoError(t, err)
	assert.Equal(t, RemoteAccessQueue, caller.Access)

	_, err = tailscaleWhois(script)(context.Background(), "100.64.0.9:1")
	require.ErrorContains(t, err, "tailscale whois failed for 100.64.0.9:1")
	require.ErrorContains(t, err, "bad args")
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/daemon -run 'TestParseWhois|TestTailscaleWhois'`
Expected: compile failure.

- [ ] **Step 3: Implement `remote_whois.go`**

```go
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
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/daemon -run 'TestParseWhois|TestTailscaleWhois'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/remote_whois.go internal/daemon/remote_whois_test.go
git commit -m "feat(daemon): identify tailnet callers with tailscale whois"
```

---

### Task 4: Git operations for remote commits and pack import

**Files:**
- Create: `internal/daemon/remote_git.go`
- Test: `internal/daemon/remote_git_test.go`

**Interfaces:**
- Produces:
  - `var errRemoteRefNotSHA = errors.New("remote enqueue needs full commit SHAs")`
  - `func parseRemoteGitRef(ref string) ([]string, error)`: accepts `<sha>`,
    `<sha>..<sha>`, and `<sha>^..<sha>`, and returns the named SHAs (never
    `<sha>^`).
  - `func isFullSHA(s string) bool`
  - `func ensureRemoteCommits(ctx context.Context, repoRoot string, shas []string) ([]string, error)`:
    returns the SHAs still missing after at most one `git fetch --all --quiet`.
  - `func daemonHaves(ctx context.Context, repoRoot string) ([]string, error)`:
    sorted, distinct, peeled commit SHAs at ref tips.
  - `const uploadRefPrefix = "refs/roborev/uploads/"`
  - `func pruneUploadRefs(ctx context.Context, repoRoot string)`
  - `type missingBaseError struct{ tip string }` (its `Error()` returns
    `daemon clone lacks base commits for <tip>; fetch on the daemon host`)
  - `func importPack(ctx context.Context, repoRoot string, pack io.Reader, tips []string) error`

- [ ] **Step 1: Write the failing tests**

```go
package daemon

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

const shaA = "0123456789abcdef0123456789abcdef01234567"
const shaB = "89abcdef0123456789abcdef0123456789abcdef"

func TestParseRemoteGitRef(t *testing.T) {
	tests := []struct {
		ref     string
		want    []string
		wantErr bool
	}{
		{ref: shaA, want: []string{shaA}},
		{ref: shaA + ".." + shaB, want: []string{shaA, shaB}},
		{ref: shaA + "^.." + shaB, want: []string{shaA, shaB}},
		{ref: "HEAD", wantErr: true},
		{ref: "main.." + shaB, wantErr: true},
		{ref: shaA[:12], wantErr: true},
		{ref: "dirty", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := parseRemoteGitRef(tt.ref)
			if tt.wantErr {
				require.ErrorIs(t, err, errRemoteRefNotSHA)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// gitOut runs git in dir with a fixed test identity and returns trimmed
// stdout. The fixtures use plain directories because testutil.InitTestGitRepo
// copies a template into its directory and would overwrite a clone.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// remoteGitFixture has a bare upstream, a daemon clone of it, and a laptop
// clone with one extra unpushed commit.
type remoteGitFixture struct {
	daemonDir, laptopDir string
	unpushed             string
}

func newRemoteGitFixture(t *testing.T) remoteGitFixture {
	t.Helper()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	gitOut(t, root, "init", "-q", "--bare", "-b", "main", upstream)
	seed := testutil.NewGitRepo(t)
	seed.CommitFile("base.txt", "base", "base")
	gitOut(t, seed.Path(), "push", "-q", upstream, "HEAD:refs/heads/main")

	f := remoteGitFixture{
		daemonDir: filepath.Join(root, "daemon"),
		laptopDir: filepath.Join(root, "laptop"),
	}
	gitOut(t, root, "clone", "-q", upstream, f.daemonDir)
	gitOut(t, root, "clone", "-q", upstream, f.laptopDir)
	gitOut(t, f.laptopDir, "commit", "-q", "--allow-empty", "-m", "unpushed")
	f.unpushed = gitOut(t, f.laptopDir, "rev-parse", "HEAD")
	return f
}

func buildPack(t *testing.T, dir string, tips, exclude []string) []byte {
	t.Helper()
	var revs strings.Builder
	for _, sha := range tips {
		revs.WriteString(sha + "\n")
	}
	for _, sha := range exclude {
		revs.WriteString("^" + sha + "\n")
	}
	cmd := exec.Command("git", "-C", dir, "pack-objects", "--revs", "--stdout", "-q")
	cmd.Stdin = strings.NewReader(revs.String())
	out, err := cmd.Output()
	require.NoError(t, err)
	return out
}

func TestEnsureRemoteCommitsFetchesOnce(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()

	missing, err := ensureRemoteCommits(ctx, f.daemonDir, []string{f.unpushed})
	require.NoError(t, err)
	assert.Equal(t, []string{f.unpushed}, missing, "unpushed commit stays missing")

	gitOut(t, f.laptopDir, "push", "-q", "origin", "HEAD:refs/heads/main")
	missing, err = ensureRemoteCommits(ctx, f.daemonDir, []string{f.unpushed})
	require.NoError(t, err)
	assert.Empty(t, missing, "fetch picks up the pushed commit")
}

func TestMissingCommitsReportsBrokenRepo(t *testing.T) {
	_, err := missingCommits(context.Background(), t.TempDir(), []string{shaA})
	require.Error(t, err, "a directory that is not a repo is an error, not a missing commit")
}

func TestDaemonHavesPeelsTags(t *testing.T) {
	f := newRemoteGitFixture(t)
	head := gitOut(t, f.daemonDir, "rev-parse", "HEAD")
	gitOut(t, f.daemonDir, "tag", "-a", "v1", "-m", "annotated")
	tree := gitOut(t, f.daemonDir, "rev-parse", "HEAD^{tree}")
	gitOut(t, f.daemonDir, "tag", "tree-tag", tree)

	haves, err := daemonHaves(context.Background(), f.daemonDir)
	require.NoError(t, err)
	assert.Contains(t, haves, head)
	assert.NotContains(t, haves, tree)
	for _, sha := range haves {
		assert.Equal(t, "commit", gitOut(t, f.daemonDir, "cat-file", "-t", sha))
	}
}

func TestImportPackPinsTips(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()
	haves, err := daemonHaves(ctx, f.daemonDir)
	require.NoError(t, err)

	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, haves)
	require.NoError(t, importPack(ctx, f.daemonDir, bytes.NewReader(pack), []string{f.unpushed}))

	assert.Equal(t, f.unpushed, gitOut(t, f.daemonDir, "rev-parse", uploadRefPrefix+f.unpushed))
	missing, err := ensureRemoteCommits(ctx, f.daemonDir, []string{f.unpushed})
	require.NoError(t, err)
	assert.Empty(t, missing)
}

func TestImportPackMissingBase(t *testing.T) {
	f := newRemoteGitFixture(t)
	gitOut(t, f.laptopDir, "commit", "-q", "--allow-empty", "-m", "second unpushed")
	second := gitOut(t, f.laptopDir, "rev-parse", "HEAD")
	// Exclude the first unpushed commit, which the daemon does not have.
	pack := buildPack(t, f.laptopDir, []string{second}, []string{f.unpushed})

	err := importPack(context.Background(), f.daemonDir, bytes.NewReader(pack), []string{second})
	var baseErr *missingBaseError
	require.ErrorAs(t, err, &baseErr)
	assert.Contains(t, err.Error(), "daemon clone lacks base commits for "+second)
	assert.Empty(t, gitOut(t, f.daemonDir, "for-each-ref", uploadRefPrefix))
}

func TestPruneUploadRefsAfterPush(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()
	haves, err := daemonHaves(ctx, f.daemonDir)
	require.NoError(t, err)
	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, haves)
	require.NoError(t, importPack(ctx, f.daemonDir, bytes.NewReader(pack), []string{f.unpushed}))

	gitOut(t, f.laptopDir, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitOut(t, f.daemonDir, "fetch", "-q", "--all")
	pruneUploadRefs(ctx, f.daemonDir)
	assert.Empty(t, gitOut(t, f.daemonDir, "for-each-ref", uploadRefPrefix))
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/daemon -run 'TestParseRemoteGitRef|TestEnsureRemoteCommits|TestDaemonHaves|TestImportPack|TestPruneUploadRefs'`
Expected: compile failure.

- [ ] **Step 3: Implement `remote_git.go`**

```go
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

var errRemoteRefNotSHA = errors.New("remote enqueue needs full commit SHAs")

var fullSHAPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func isFullSHA(s string) bool { return fullSHAPattern.MatchString(s) }

// parseRemoteGitRef returns the commits a remote enqueue names. Remote
// callers must send full SHAs because symbolic refs would resolve in the
// daemon's clone rather than the caller's. The inclusive "<sha>^..<sha>"
// form names <sha> itself; the enqueue handler's empty-tree fallback covers
// a root <sha>.
func parseRemoteGitRef(ref string) ([]string, error) {
	start, end, isRange := strings.Cut(ref, "..")
	if !isRange {
		if !isFullSHA(ref) {
			return nil, errRemoteRefNotSHA
		}
		return []string{ref}, nil
	}
	start = strings.TrimSuffix(start, "^")
	if !isFullSHA(start) || !isFullSHA(end) {
		return nil, errRemoteRefNotSHA
	}
	return []string{start, end}, nil
}

// missingCommits returns the SHAs the clone lacks. rev-parse --verify
// --quiet exits 1 for a missing object and 128 for a real failure, such as
// a path that is not a repository, so only exit 1 counts as missing.
func missingCommits(ctx context.Context, repoRoot string, shas []string) ([]string, error) {
	var missing []string
	for _, sha := range shas {
		_, _, err := gitcmd.New().Run(ctx, repoRoot, nil,
			"rev-parse", "--verify", "--quiet", sha+"^{commit}")
		if err == nil {
			continue
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			missing = append(missing, sha)
			continue
		}
		return nil, fmt.Errorf("check commit %s in %s: %w", sha, repoRoot, err)
	}
	return missing, nil
}

// ensureRemoteCommits fetches once from the clone's configured remotes when
// any named commit is missing, and returns the commits still missing.
func ensureRemoteCommits(ctx context.Context, repoRoot string, shas []string) ([]string, error) {
	missing, err := missingCommits(ctx, repoRoot, shas)
	if err != nil || len(missing) == 0 {
		return missing, err
	}
	if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "fetch", "--all", "--quiet"); err != nil {
		log.Printf("remote enqueue: fetch in %s failed: %v", repoRoot, err)
	} else {
		pruneUploadRefs(ctx, repoRoot)
	}
	return missingCommits(ctx, repoRoot, shas)
}

// daemonHaves lists the distinct commits at the clone's ref tips, peeling
// annotated tags and skipping refs that point at non-commits.
func daemonHaves(ctx context.Context, repoRoot string) ([]string, error) {
	out, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref",
		"--format=%(objecttype) %(objectname) %(*objecttype) %(*objectname)")
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "commit":
			seen[fields[1]] = true
		case len(fields) == 4 && fields[2] == "commit":
			seen[fields[3]] = true
		}
	}
	haves := make([]string, 0, len(seen))
	for sha := range seen {
		haves = append(haves, sha)
	}
	slices.Sort(haves)
	return haves, nil
}

const uploadRefPrefix = "refs/roborev/uploads/"

// pruneUploadRefs drops upload refs whose commit a remote-tracking branch
// now contains. Failures are logged because pruning is housekeeping.
func pruneUploadRefs(ctx context.Context, repoRoot string) {
	out, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref", "--format=%(objectname)", uploadRefPrefix)
	if err != nil {
		log.Printf("remote uploads: list upload refs in %s: %v", repoRoot, err)
		return
	}
	for sha := range strings.FieldsSeq(string(out)) {
		contains, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref",
			"--count=1", "--contains", sha, "--format=%(refname)", "refs/remotes/")
		if err != nil {
			log.Printf("remote uploads: check %s in %s: %v", sha, repoRoot, err)
			continue
		}
		if strings.TrimSpace(string(contains)) == "" {
			continue
		}
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "update-ref", "-d", uploadRefPrefix+sha); err != nil {
			log.Printf("remote uploads: delete %s in %s: %v", uploadRefPrefix+sha, repoRoot, err)
		}
	}
}

type missingBaseError struct{ tip string }

func (e *missingBaseError) Error() string {
	return fmt.Sprintf("daemon clone lacks base commits for %s; fetch on the daemon host", e.tip)
}

// importPack stores a caller-supplied git pack in the clone's object store
// and pins each tip under refs/roborev/uploads/. It never touches the
// working tree, the index, branches, or remote-tracking refs.
func importPack(ctx context.Context, repoRoot string, pack io.Reader, tips []string) error {
	if _, _, err := gitcmd.New().Run(ctx, repoRoot, pack, "index-pack", "--stdin"); err != nil {
		return fmt.Errorf("index pack: %w", err)
	}
	for _, tip := range tips {
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil,
			"rev-list", "--quiet", "--objects", tip, "--not", "--all"); err != nil {
			return &missingBaseError{tip: tip}
		}
	}
	for _, tip := range tips {
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "update-ref", uploadRefPrefix+tip, tip); err != nil {
			return fmt.Errorf("pin %s: %w", tip, err)
		}
	}
	return nil
}
```

`gitcmd.GitError` implements `Unwrap`, so `errors.AsType[*exec.ExitError]`
reaches the process exit status. The module targets Go 1.27.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/daemon -run 'TestParseRemoteGitRef|TestEnsureRemoteCommits|TestDaemonHaves|TestImportPack|TestPruneUploadRefs'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/remote_git.go internal/daemon/remote_git_test.go
git commit -m "feat(daemon): fetch, negotiate, and import commits for remote callers"
```

---

### Task 5: Remote handler: auth, allowlist, repo mapping, enqueue, rerun, and pack upload

**Files:**
- Create: `internal/daemon/remote_handler.go`
- Modify: `internal/daemon/types.go` (add `RepoIdentity` to `EnqueueRequest`
  and add `MissingCommitsResponse`)
- Modify: `internal/daemon/server.go` (`humaEnqueue`: branch handling for
  remote callers, ~2718-2747)
- Regenerate: `pkg/client/openapi.yaml`, `pkg/client/generated/*`, and the web
  generated types (`make api-generate`)
- Test: `internal/daemon/remote_handler_test.go`

**Interfaces:**
- Consumes: Task 2 `FindReposByIdentity`, Task 3 `RemoteCaller`/`whoisFunc`,
  Task 4 helpers, and Task 4 test helpers `gitOut`, `newRemoteGitFixture`,
  `buildPack`, `shaA`.
- Produces:
  - `EnqueueRequest.RepoIdentity string` (`json:"repo_identity,omitempty"`)
  - `const MissingCommitsCode = "missing_commits"`
  - `type MissingCommitsResponse struct{ Error string; Code string; Missing []string; Have []string }`
    with JSON tags `error`, `code`, `missing`, `have`
  - `const RemotePackPath = "/api/remote/pack"`
  - `func RemoteCallerFromContext(ctx context.Context) (RemoteCaller, bool)`
  - `func remoteConnContext(ctx context.Context, _ net.Conn) context.Context`
  - `func (s *Server) newRemoteHandler(core http.Handler, whois whoisFunc) http.Handler`

- [ ] **Step 1: Write the failing tests**

```go
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func fakeWhois(access RemoteAccess, err error) whoisFunc {
	return func(context.Context, string) (RemoteCaller, error) {
		return RemoteCaller{Node: "laptop.", Login: "user-a@example.com", Access: access}, err
	}
}

func serveRemote(t *testing.T, s *Server, whois whoisFunc, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.RemoteAddr = "100.64.0.2:5555"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(remoteConnContext(req.Context(), nil))
	w := httptest.NewRecorder()
	s.newRemoteHandler(s.httpServer.Handler, whois).ServeHTTP(w, req)
	return w
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body.Error
}

func TestRemoteHandlerAuthAndAllowlist(t *testing.T) {
	server, _, _ := newTestServer(t)
	tests := []struct {
		name       string
		whois      whoisFunc
		method     string
		target     string
		wantStatus int
		wantError  string
	}{
		{"whois failure", fakeWhois(RemoteAccessNone, errors.New("tailscale whois failed for 100.64.0.2:5555: no peer")),
			http.MethodGet, "/api/status", http.StatusForbidden, "tailscale whois failed"},
		{"no grant", fakeWhois(RemoteAccessNone, nil),
			http.MethodGet, "/api/status", http.StatusForbidden, "tailnet policy grants this node no roborev access"},
		{"read can read", fakeWhois(RemoteAccessRead, nil),
			http.MethodGet, "/api/status", http.StatusOK, ""},
		{"read cannot close", fakeWhois(RemoteAccessRead, nil),
			http.MethodPost, "/api/review/close", http.StatusForbidden, "/api/review/close requires queue access; this node has read access"},
		{"queue denied admin route", fakeWhois(RemoteAccessQueue, nil),
			http.MethodPost, "/api/shutdown", http.StatusForbidden, "/api/shutdown is not available over the remote API; run it on the daemon host"},
		{"mcp denied", fakeWhois(RemoteAccessQueue, nil),
			http.MethodPost, "/mcp", http.StatusForbidden, "is not available over the remote API"},
		{"prefix filter denied", fakeWhois(RemoteAccessRead, nil),
			http.MethodGet, "/api/jobs?repo_prefix=/srv", http.StatusBadRequest, "path-prefix filters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveRemote(t, server, tt.whois, tt.method, tt.target, nil)
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.wantError != "" {
				assert.Contains(t, errorBody(t, w), tt.wantError)
			}
		})
	}
}

func TestRemoteHandlerRepoFilters(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	testutil.InitTestGitRepo(t, repoDir)
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)
	read := fakeWhois(RemoteAccessRead, nil)

	w := serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo="+id, nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo="+repoDir, nil)
	assert.Equal(t, http.StatusOK, w.Code, "registered root_path passes through: "+w.Body.String())

	w = serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo=https://example.com/org/unknown.git", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, errorBody(t, w), "is not registered on the daemon host")
}

func TestRemoteHandlerRepoIdentityResolution(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	const id = "https://example.com/org/dup.git"
	_, err := db.GetOrCreateRepoByIdentity(id) // sync placeholder: never matches
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: shaA, Agent: "test"})

	w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(tmpDir, name)
		testutil.InitTestGitRepo(t, dir)
		_, err := db.GetOrCreateRepo(dir, id)
		require.NoError(t, err)
	}
	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "matches several daemon checkouts")
}

func TestRemoteEnqueueValidation(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	repo := testutil.InitTestGitRepo(t, repoDir)
	head := repo.CommitFile("a.txt", "a", "a")
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)

	tests := []struct {
		name       string
		req        EnqueueRequest
		wantStatus int
		wantError  string
	}{
		{"repo_path rejected", EnqueueRequest{RepoPath: repoDir, RepoIdentity: id, GitRef: head}, http.StatusBadRequest, "repo_path is not accepted"},
		{"identity required", EnqueueRequest{GitRef: head}, http.StatusBadRequest, "repo_identity is required"},
		{"dirty", EnqueueRequest{RepoIdentity: id, GitRef: "dirty", DiffContent: "diff"}, http.StatusForbidden, "dirty reviews need a local daemon"},
		{"custom prompt", EnqueueRequest{RepoIdentity: id, GitRef: head, CustomPrompt: "do things"}, http.StatusForbidden, "task reviews need a local daemon"},
		{"agentic", EnqueueRequest{RepoIdentity: id, GitRef: head, Agentic: true}, http.StatusForbidden, "agentic reviews need a local daemon"},
		{"fix job type", EnqueueRequest{RepoIdentity: id, GitRef: head, JobType: storage.JobTypeFix}, http.StatusForbidden, "fix reviews need a local daemon"},
		{"symbolic ref", EnqueueRequest{RepoIdentity: id, GitRef: "HEAD"}, http.StatusBadRequest, "remote enqueue needs full commit SHAs"},
		{"bad branch", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "feat..x"}, http.StatusBadRequest, "invalid branch name"},
		{"dash branch", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "-x"}, http.StatusBadRequest, "invalid branch name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.req)
			w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Contains(t, errorBody(t, w), tt.wantError)
		})
	}
}

func TestRemoteEnqueueMissingCommitsAndSuccess(t *testing.T) {
	server, db, _ := newTestServer(t)
	f := newRemoteGitFixture(t)
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(f.daemonDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: f.unpushed, Branch: "feature-x", Agent: "test"})

	w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var missing MissingCommitsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &missing))
	assert.Equal(t, MissingCommitsCode, missing.Code)
	assert.Equal(t, []string{f.unpushed}, missing.Missing)
	assert.Contains(t, missing.Have, gitOut(t, f.daemonDir, "rev-parse", "HEAD"))

	pack := buildPack(t, f.laptopDir, missing.Missing, missing.Have)
	w = serveRemote(t, server, queue, http.MethodPost,
		RemotePackPath+"?repo_identity="+id+"&tip="+f.unpushed, pack)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &job))
	assert.Equal(t, "feature-x", job.Branch, "request branch wins over the daemon checkout branch")
}

func TestRemoteEnqueueEmptyBranchIgnoresCheckoutBranch(t *testing.T) {
	server, db, _ := newTestServer(t)
	f := newRemoteGitFixture(t)
	gitOut(t, f.daemonDir, "checkout", "-q", "-b", "daemon-local")
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(f.daemonDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, []string{gitOut(t, f.daemonDir, "rev-parse", "HEAD")})
	w := serveRemote(t, server, queue, http.MethodPost,
		RemotePackPath+"?repo_identity="+id+"&tip="+f.unpushed, pack)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: f.unpushed, Agent: "test"})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &job))
	// Inference against the clone's refs may name a branch or leave it
	// empty; the daemon checkout's own branch must never be used.
	assert.NotEqual(t, "daemon-local", job.Branch)
}

func TestRemoteEnqueueRootInclusiveRange(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	repo := testutil.InitTestGitRepo(t, repoDir)
	root := repo.HeadSHA() // InitTestGitRepo's initial commit has no parent
	head := repo.CommitFile("b.txt", "b", "second")
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)

	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: root + "^.." + head, Branch: "main", Agent: "test"})
	w := serveRemote(t, server, fakeWhois(RemoteAccessQueue, nil), http.MethodPost, "/api/enqueue", body)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

func TestRemoteRerunEligibility(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	// Same setup as TestHumaRerunJob: tmpDir is a real directory for rerun
	// validation, and failed jobs are rerunnable.
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "deadbeef", "A", "S", time.Now())
	require.NoError(t, err)
	review, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "deadbeef", Agent: "test",
	})
	require.NoError(t, err)
	task, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "deadbeef", Agent: "test",
		JobType: storage.JobTypeTask, Prompt: "hello",
	})
	require.NoError(t, err)
	for range 2 {
		claimed, err := db.ClaimJob("w")
		require.NoError(t, err)
		_, err = db.FailJob(claimed.ID, "", "some error")
		require.NoError(t, err)
	}
	queue := fakeWhois(RemoteAccessQueue, nil)

	body, _ := json.Marshal(map[string]any{"job_id": task.ID})
	w := serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "need a local daemon")

	body, _ = json.Marshal(map[string]any{"job_id": review.ID})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
```

Add `"time"` to the test imports.

Also add a panel case to `TestRemoteRerunEligibility`. Build a finished panel
run the way the existing panel rerun tests in `rerun_panel_test.go` do. Rerun
its synthesis job remotely and expect `200`. Then set `agentic = 1` on one
member row with a direct `UPDATE review_jobs` and expect `403`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/daemon -run 'TestRemoteHandler|TestRemoteEnqueue|TestRemoteRerun'`
Expected: compile failure.

- [ ] **Step 3: Implement**

`types.go`: add to `EnqueueRequest`:

```go
	RepoIdentity      string   `json:"repo_identity,omitempty"`       // Remote callers: repo identity instead of repo_path
```

and:

```go
// MissingCommitsCode marks a remote enqueue that named commits the daemon
// clone lacks even after fetching.
const MissingCommitsCode = "missing_commits"

// MissingCommitsResponse lists the missing commits and the commits at the
// daemon clone's ref tips, so the caller can pack only what is missing.
type MissingCommitsResponse struct {
	Error   string   `json:"error"`
	Code    string   `json:"code"`
	Missing []string `json:"missing"`
	Have    []string `json:"have"`
}
```

`server.go` `humaEnqueue`: replace the branch-selection block (~2718-2747) so
that remote requests use the request branch for exclusion and never default
to the checkout branch:

```go
	currentBranch := metadata.CurrentBranch()
	_, remoteCaller := RemoteCallerFromContext(ctx)
	// ... keep the existing comment ...
	branchToCheck := currentBranch
	if remoteCaller {
		// A remote caller's work has nothing to do with the daemon
		// checkout's branch; only the branch it reported applies.
		branchToCheck = req.Branch
	} else if req.Source == "post_commit" && req.Branch != "" {
		branchToCheck = req.Branch
	} else if req.JobType == storage.JobTypeInsights {
		// unchanged
	}
	// unchanged exclusion check
	if req.Branch == "" && req.JobType != storage.JobTypeInsights && !remoteCaller {
		req.Branch = currentBranch
	}
```

The detached-HEAD inference block below it stays unchanged, so an empty
remote branch is still inferred from the daemon clone's refs.

`remote_handler.go`:

```go
package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	gitcmd "go.kenn.io/kit/git/cmd"

	"go.kenn.io/roborev/internal/storage"
)

// RemotePackPath receives git packs of commits the daemon clone lacks.
const RemotePackPath = "/api/remote/pack"

// remoteAPIRoutes lists every route the remote listener serves and the
// minimum access level each needs. Everything else is refused.
var remoteAPIRoutes = map[routeKey]RemoteAccess{
	{http.MethodGet, "/api/ping"}:          RemoteAccessRead,
	{http.MethodGet, "/api/status"}:        RemoteAccessRead,
	{http.MethodGet, "/api/health"}:        RemoteAccessRead,
	{http.MethodGet, "/api/jobs"}:          RemoteAccessRead,
	{http.MethodGet, "/api/review"}:        RemoteAccessRead,
	{http.MethodGet, "/api/search"}:        RemoteAccessRead,
	{http.MethodGet, "/api/comments"}:      RemoteAccessRead,
	{http.MethodGet, "/api/repos"}:         RemoteAccessRead,
	{http.MethodGet, "/api/branches"}:      RemoteAccessRead,
	{http.MethodGet, "/api/summary"}:       RemoteAccessRead,
	{http.MethodGet, "/api/cost"}:          RemoteAccessRead,
	{http.MethodGet, "/api/activity"}:      RemoteAccessRead,
	{http.MethodGet, "/api/job/output"}:    RemoteAccessRead,
	{http.MethodGet, "/api/job/log"}:       RemoteAccessRead,
	{http.MethodGet, "/api/stream/events"}: RemoteAccessRead,
	{http.MethodPost, "/api/jobs/batch"}:   RemoteAccessRead,
	{http.MethodPost, "/api/enqueue"}:      RemoteAccessQueue,
	{http.MethodPost, "/api/job/cancel"}:   RemoteAccessQueue,
	{http.MethodPost, "/api/job/rerun"}:    RemoteAccessQueue,
	{http.MethodPost, "/api/review/close"}: RemoteAccessQueue,
	{http.MethodPost, "/api/comment"}:      RemoteAccessQueue,
	{http.MethodPost, RemotePackPath}:      RemoteAccessQueue,
}

type remoteCallerContextKey struct{}

// RemoteCallerFromContext reports the tailnet caller of a request that
// arrived on the remote listener.
func RemoteCallerFromContext(ctx context.Context) (RemoteCaller, bool) {
	caller, ok := ctx.Value(remoteCallerContextKey{}).(RemoteCaller)
	return caller, ok
}

// remoteConnAuth caches one whois result per TCP connection, so a revoked
// grant takes effect on the peer's next connection.
type remoteConnAuth struct {
	once   sync.Once
	caller RemoteCaller
	err    error
}

type remoteConnAuthKey struct{}

func remoteConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, remoteConnAuthKey{}, &remoteConnAuth{})
}

type remoteError struct {
	status int
	msg    string
}

func (e *remoteError) Error() string { return e.msg }

func newRemoteError(status int, format string, args ...any) *remoteError {
	return &remoteError{status: status, msg: fmt.Sprintf(format, args...)}
}

func writeRemoteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeRemoteError(w http.ResponseWriter, err error) {
	var re *remoteError
	if errors.As(err, &re) {
		writeRemoteJSON(w, re.status, ErrorResponse{Error: re.msg})
		return
	}
	writeRemoteJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
}

func authenticateRemote(r *http.Request, whois whoisFunc) (RemoteCaller, error) {
	auth, ok := r.Context().Value(remoteConnAuthKey{}).(*remoteConnAuth)
	if !ok {
		return RemoteCaller{}, errors.New("remote connection has no identity state")
	}
	auth.once.Do(func() {
		auth.caller, auth.err = whois(r.Context(), r.RemoteAddr)
	})
	return auth.caller, auth.err
}

func (s *Server) newRemoteHandler(core http.Handler, whois whoisFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := authenticateRemote(r, whois)
		if err != nil {
			writeRemoteError(w, newRemoteError(http.StatusForbidden, "%v", err))
			return
		}
		if caller.Access == RemoteAccessNone {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"tailnet policy grants this node no roborev access"))
			return
		}
		need, found := remoteAPIRoutes[routeKey{r.Method, r.URL.Path}]
		if !found {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"%s is not available over the remote API; run it on the daemon host", r.URL.Path))
			return
		}
		if caller.Access < need {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"%s requires %s access; this node has %s access", r.URL.Path, need, caller.Access))
			return
		}
		if r.Method != http.MethodGet {
			log.Printf("remote %s %s by node=%s login=%s tags=%v access=%s",
				r.Method, r.URL.Path, caller.Node, caller.Login, caller.Tags, caller.Access)
		}
		r = r.WithContext(context.WithValue(r.Context(), remoteCallerContextKey{}, caller))
		switch r.URL.Path {
		case "/api/enqueue":
			s.serveRemoteEnqueue(w, r, core)
		case "/api/job/rerun":
			s.serveRemoteRerun(w, r, core)
		case RemotePackPath:
			s.serveRemotePack(w, r)
		default:
			if err := s.rewriteRemoteRepoFilters(r); err != nil {
				writeRemoteError(w, err)
				return
			}
			core.ServeHTTP(w, r)
		}
	})
}

// resolveRemoteRepo maps a repo identity to the single registered checkout
// with that identity.
func (s *Server) resolveRemoteRepo(identity string) (*storage.Repo, error) {
	repos, err := s.db.FindReposByIdentity(identity)
	if err != nil {
		return nil, fmt.Errorf("look up repo %s: %w", identity, err)
	}
	switch len(repos) {
	case 0:
		return nil, newRemoteError(http.StatusNotFound,
			"repo %s is not registered on the daemon host; run roborev init there, or add a matching .roborev-id",
			identity)
	case 1:
		return &repos[0], nil
	default:
		paths := make([]string, len(repos))
		for i, repo := range repos {
			paths[i] = repo.RootPath
		}
		return nil, newRemoteError(http.StatusConflict,
			"repo %s matches several daemon checkouts: %s", identity, strings.Join(paths, ", "))
	}
}

// rewriteRemoteRepoFilters turns identity values of the repo filter into
// daemon root paths. Exact registered root paths (which the caller got from
// /api/repos) pass through unchanged.
func (s *Server) rewriteRemoteRepoFilters(r *http.Request) error {
	q := r.URL.Query()
	if q.Has("repo_prefix") || (r.URL.Path == "/api/repos" && q.Has("prefix")) {
		return newRemoteError(http.StatusBadRequest,
			"path-prefix filters are not available over the remote API; filter by repo identity")
	}
	values := q["repo"]
	if len(values) == 0 {
		return nil
	}
	rewritten := make([]string, 0, len(values))
	for _, value := range values {
		// Only absolute values can be daemon root paths. GetRepoByPath would
		// resolve a relative identity against the daemon's working directory.
		if filepath.IsAbs(value) {
			repo, err := s.db.GetRepoByPath(value)
			switch {
			case err == nil && repo.RootPath != repo.Identity:
				rewritten = append(rewritten, repo.RootPath)
				continue
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("look up repo path %s: %w", value, err)
			}
		}
		repo, err := s.resolveRemoteRepo(value)
		if err != nil {
			return err
		}
		rewritten = append(rewritten, repo.RootPath)
	}
	q["repo"] = rewritten
	r.URL.RawQuery = q.Encode()
	return nil
}

func validateRemoteEnqueue(req *EnqueueRequest) error {
	if req.RepoPath != "" {
		return newRemoteError(http.StatusBadRequest,
			"repo_path is not accepted over the remote API; send repo_identity")
	}
	if req.RepoIdentity == "" {
		return newRemoteError(http.StatusBadRequest, "repo_identity is required")
	}
	kind := ""
	switch {
	case req.CustomPrompt != "":
		kind = storage.JobTypeTask
	case req.Agentic:
		kind = "agentic"
	case req.GitRef == "dirty" || req.DiffContent != "" || len(req.DirtyFiles) > 0:
		kind = storage.JobTypeDirty
	case req.Since != "" || req.JobType == storage.JobTypeInsights:
		kind = storage.JobTypeInsights
	case req.AnalysisType != "" || len(req.AnalysisFiles) > 0 || req.AnalysisCommitSHA != "":
		kind = "analyze"
	case req.JobType != "" && req.JobType != storage.JobTypeReview && req.JobType != storage.JobTypeRange:
		kind = req.JobType
	}
	if kind != "" {
		return newRemoteError(http.StatusForbidden, "%s reviews need a local daemon", kind)
	}
	if req.Branch != "" && !validBranchName(req.Branch) {
		return newRemoteError(http.StatusBadRequest, "invalid branch name %q", req.Branch)
	}
	return nil
}

func validBranchName(name string) bool {
	if strings.HasPrefix(name, "-") {
		return false
	}
	_, _, err := gitcmd.New().Run(context.Background(), "", nil, "check-ref-format", "--branch", name)
	return err == nil
}

func (s *Server) serveRemoteEnqueue(w http.ResponseWriter, r *http.Request, core http.Handler) {
	var req EnqueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "decode enqueue request: %v", err))
		return
	}
	if req.GitRef == "" {
		req.GitRef, req.CommitSHA = req.CommitSHA, ""
	}
	if err := validateRemoteEnqueue(&req); err != nil {
		writeRemoteError(w, err)
		return
	}
	shas, err := parseRemoteGitRef(req.GitRef)
	if err != nil {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "%v", err))
		return
	}
	repo, err := s.resolveRemoteRepo(req.RepoIdentity)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	missing, err := ensureRemoteCommits(r.Context(), repo.RootPath, shas)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	if len(missing) > 0 {
		haves, err := daemonHaves(r.Context(), repo.RootPath)
		if err != nil {
			writeRemoteError(w, err)
			return
		}
		writeRemoteJSON(w, http.StatusConflict, MissingCommitsResponse{
			Error:   "commits are missing from the daemon clone; upload them and retry",
			Code:    MissingCommitsCode,
			Missing: missing,
			Have:    haves,
		})
		return
	}
	req.RepoPath, req.RepoIdentity = repo.RootPath, ""
	body, err := json.Marshal(req)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	core.ServeHTTP(w, r)
}

func remoteReviewJob(job *storage.ReviewJob) bool {
	return job.IsReviewJob() && !job.Agentic &&
		job.JobType != storage.JobTypeDirty && job.GitRef != "dirty" && job.DiffContent == nil
}

func (s *Server) remoteRerunAllowed(job *storage.ReviewJob) (bool, error) {
	if !job.IsSynthesisJob() {
		return remoteReviewJob(job), nil
	}
	if job.PanelRunUUID == nil {
		return false, nil
	}
	members, err := s.db.GetPanelMembers(*job.PanelRunUUID)
	if err != nil {
		return false, fmt.Errorf("load panel members: %w", err)
	}
	for i := range members {
		if !remoteReviewJob(&members[i]) {
			return false, nil
		}
	}
	return len(members) > 0, nil
}

func (s *Server) serveRemoteRerun(w http.ResponseWriter, r *http.Request, core http.Handler) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "read rerun request: %v", err))
		return
	}
	var req RerunJobRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "decode rerun request: %v", err))
		return
	}
	// A missing job falls through to the core handler's 404.
	if job, err := s.db.GetJobByID(req.JobID); err == nil {
		allowed, err := s.remoteRerunAllowed(job)
		if err != nil {
			writeRemoteError(w, err)
			return
		}
		if !allowed {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"rerunning job %d needs a local daemon: only non-agentic commit and range reviews rerun remotely", req.JobID))
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	core.ServeHTTP(w, r)
}

func (s *Server) serveRemotePack(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tips := q["tip"]
	if len(tips) == 0 {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "at least one tip is required"))
		return
	}
	for _, tip := range tips {
		if !isFullSHA(tip) {
			writeRemoteError(w, newRemoteError(http.StatusBadRequest, "tip %q is not a full commit SHA", tip))
			return
		}
	}
	repo, err := s.resolveRemoteRepo(q.Get("repo_identity"))
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	if err := importPack(r.Context(), repo.RootPath, r.Body, tips); err != nil {
		var baseErr *missingBaseError
		if errors.As(err, &baseErr) {
			writeRemoteError(w, newRemoteError(http.StatusConflict, "%v", err))
			return
		}
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "%v", err))
		return
	}
	writeRemoteJSON(w, http.StatusOK, map[string][]string{"pinned": tips})
}
```

`GetRepoByPath` (repos.go:136) returns `sql.ErrNoRows` for an unknown path;
the passthrough handles that case and returns every other error. The rerun body type is
`RerunJobRequest` (types.go:294), with `JobID int64`.

Then regenerate the API clients: `make api-generate`. If `bun` is missing,
run `nix shell 'nixpkgs#bun' --command make api-generate`. Confirm with
`make api-check`.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/daemon -run 'TestRemote|TestHandleEnqueue|TestHumaRerunJob'`
Expected: PASS. Then run `go test ./internal/daemon` to catch regressions in
enqueue branch handling.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon pkg/client web
git commit -m "feat(daemon): serve the API to tailnet callers with read and queue access"
```

---

### Task 6: Start and stop the remote listener

**Files:**
- Create: `internal/daemon/remote_server.go`
- Modify: `internal/daemon/server.go` (`Server` struct fields; `Start` after
  the browser listener starts, ~402-429; `stopOnce0` shutdown block, ~645-700)
- Test: `internal/daemon/remote_server_test.go`

**Interfaces:**
- Consumes: Task 5 `newRemoteHandler`, `remoteConnContext`; Task 3
  `tailscaleWhois`.
- Produces: `Server.remoteServer *http.Server`, `Server.remoteWhois whoisFunc`
  (a test override), and
  `func (s *Server) startRemoteServer(remote config.RemoteAPIConfig) error`.

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestStartRemoteServerServesAuthenticatedRequests(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.remoteWhois = fakeWhois(RemoteAccessRead, nil)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())

	// startRemoteServer skips config normalization, so a loopback address
	// stands in for the Tailscale address here.
	require.NoError(t, server.startRemoteServer(config.RemoteAPIConfig{Enabled: true, Listen: addr}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.remoteServer.Shutdown(ctx)
	})

	resp, err := http.Get(fmt.Sprintf("http://%s/api/ping", addr))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, 5*time.Second, server.remoteServer.ReadHeaderTimeout)
	assert.Zero(t, server.remoteServer.ReadTimeout)
	assert.Zero(t, server.remoteServer.WriteTimeout)
}

func TestStartRemoteServerDisabled(t *testing.T) {
	server, _, _ := newTestServer(t)
	require.NoError(t, server.startRemoteServer(config.RemoteAPIConfig{}))
	assert.Nil(t, server.remoteServer)
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/daemon -run TestStartRemoteServer`
Expected: compile failure.

- [ ] **Step 3: Implement**

Add to `Server`:

```go
	remoteServer *http.Server
	remoteWhois  whoisFunc // tests override tailscale whois
```

`remote_server.go`:

```go
package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// startRemoteServer serves the core API to tailnet peers on the configured
// Tailscale address. It uses the browser listener's header and idle timeouts
// but no whole-request timeout: pack uploads and event streams can run long.
func (s *Server) startRemoteServer(remote config.RemoteAPIConfig) error {
	if !remote.Enabled {
		return nil
	}
	listener, err := net.Listen("tcp", remote.Listen)
	if err != nil {
		return fmt.Errorf("listen on remote_api.listen %s: %w", remote.Listen, err)
	}
	whois := s.remoteWhois
	if whois == nil {
		whois = tailscaleWhois(remote.TailscalePath)
	}
	server := &http.Server{
		Handler:           s.newRemoteHandler(s.httpServer.Handler, whois),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ConnContext:       remoteConnContext,
	}
	s.browserMu.Lock()
	if s.browserStopping {
		s.browserMu.Unlock()
		_ = listener.Close()
		return http.ErrServerClosed
	}
	s.remoteServer = server
	s.browserMu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Remote API server stopped: %v", err)
		}
	}()
	log.Printf("Remote API listening on %s (tailnet identity required)", listener.Addr())
	return nil
}
```

In `Start`, right after `startBrowserServer` succeeds and before
`s.browserMu.Lock()` stores the runtime:

```go
	if err := s.startRemoteServer(cfg.RemoteAPI); err != nil {
		_ = s.httpServer.Close()
		s.configWatcher.Stop()
		s.workerPool.Stop()
		s.stopSearch()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
```

If `startRemoteServer` fails after the browser server started, also close the
browser server. Read the error path after `startBrowserServer` and mirror its
cleanup.

In `stopOnce0`, read `remoteServer := s.remoteServer` together with the
browser fields under `browserMu`. After the browser shutdown block, add:

```go
	if remoteServer != nil {
		if err := remoteServer.Shutdown(shutdownCleanupCtx); err != nil {
			log.Printf("Remote API server shutdown error: %v", err)
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("shutdown remote API server: %w", err))
		}
	}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/daemon -run 'TestStartRemoteServer|TestServer'`
Expected: PASS. Then run `go test ./internal/daemon`.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/remote_server.go internal/daemon/remote_server_test.go internal/daemon/server.go
git commit -m "feat(daemon): start the remote API listener when remote_api is enabled"
```

---

### Task 7: CLI remote mode: endpoint selection, no local daemon management, local-only commands

**Files:**
- Create: `cmd/roborev/remote_mode.go`
- Modify: `cmd/roborev/daemon_lifecycle.go` (`validateServerFlag` :140,
  `getDaemonEndpoint` :155, `ensureDaemon` :234)
- Modify: `internal/daemon/runtime.go` (`ProbeDaemonPing` at ~462: split so a
  remote variant skips the loopback check)
- Modify: `cmd/roborev/agent_hook_client.go:186` (`agentHookEndpoint` uses
  the local resolver)
- Modify: the local-only call sites (see Step 3)
- Modify: `cmd/roborev/init_cmd.go` (~103-129), `cmd/roborev/remap.go` (~110)
- Modify: `cmd/roborev/tui_cmd.go` (~36-51), `cmd/roborev/mcp_cmd.go`
  (`ensureMCPDaemon` :22)
- Test: `cmd/roborev/remote_mode_test.go`

**Interfaces:**
- Produces:
  - `var remoteEndpoint *daemon.DaemonEndpoint`
  - `func isRemoteMode() bool`
  - `func parseRemoteServer(raw string) (daemon.DaemonEndpoint, bool, error)`:
    returns `ok=false` for loopback hosts
  - `func resolveRemoteEndpoint() error`
  - `func localDaemonEndpoint() daemon.DaemonEndpoint`: today's
    `getDaemonEndpoint` body
  - `func requireLocalDaemon(command string) error`
  - `func ensureLocalDaemon(command string) error`
  - `func daemon.ProbeRemoteDaemonPing(ep daemon.DaemonEndpoint, timeout time.Duration) (*daemon.PingInfo, error)`
  - `var probeRemoteDaemon = daemon.ProbeRemoteDaemonPing`
  - `func ensureRemoteDaemon() error`
  - test helper `func withRemoteState(t *testing.T)` (saves and restores
    `serverAddr`, `remoteEndpoint`, and the daemon start/probe hooks, and sets
    `ROBOREV_DATA_DIR`), used by Tasks 8 and 9

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestParseRemoteServer(t *testing.T) {
	tests := []struct {
		raw        string
		wantRemote bool
		wantAddr   string
		wantErr    string
	}{
		{raw: "http://daemon-host.example:7474", wantRemote: true, wantAddr: "daemon-host.example:7474"},
		{raw: "http://100.64.0.5:7474", wantRemote: true, wantAddr: "100.64.0.5:7474"},
		{raw: "http://127.0.0.1:7373"},
		{raw: "http://localhost:7373"},
		{raw: "http://[::1]:7373"},
		{raw: "https://daemon-host.example:7474", wantErr: "must look like http://host:port"},
		{raw: "http://daemon-host.example", wantErr: "needs a port"},
		{raw: "http://daemon-host.example:7474/api", wantErr: "must look like http://host:port"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			ep, remote, err := parseRemoteServer(tt.raw)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRemote, remote)
			if tt.wantRemote {
				assert.Equal(t, daemon.DaemonEndpoint{Network: "tcp", Address: tt.wantAddr}, ep)
			}
		})
	}
}

func withRemoteState(t *testing.T) {
	t.Helper()
	origServer, origRemote := serverAddr, remoteEndpoint
	origStart, origRestart, origProbe := startDaemonForEnsure, restartDaemonForEnsure, probeRemoteDaemon
	t.Cleanup(func() {
		serverAddr, remoteEndpoint = origServer, origRemote
		startDaemonForEnsure, restartDaemonForEnsure, probeRemoteDaemon = origStart, origRestart, origProbe
	})
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
}

func TestRemoteModeFromConfig(t *testing.T) {
	withRemoteState(t)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "config.toml"),
		[]byte("[remote]\nserver = \"http://daemon-host.example:7474\"\n"), 0o644))
	serverAddr = ""
	require.NoError(t, validateServerFlag())
	require.True(t, isRemoteMode())
	assert.Equal(t, "daemon-host.example:7474", getDaemonEndpoint().Address)

	serverAddr = "http://127.0.0.1:7373" // a loopback flag overrides the config
	require.NoError(t, validateServerFlag())
	assert.False(t, isRemoteMode())
}

func TestEnsureDaemonRemoteNeverStartsLocal(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	startDaemonForEnsure = func() error { panic("remote mode must not start a local daemon") }
	restartDaemonForEnsure = func() error { panic("remote mode must not restart a local daemon") }

	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{Version: "some-other-version"}, nil
	}
	require.NoError(t, ensureDaemon(), "remote mode skips the version check")

	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, errors.New("connection refused")
	}
	err := ensureDaemon()
	require.ErrorContains(t, err, "remote daemon at http://daemon-host.example:7474 is not reachable")
	require.ErrorContains(t, err, "connection refused")
}

func TestLocalOnlyCommandsFailInRemoteMode(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	err := ensureLocalDaemon("roborev fix")
	require.ErrorContains(t, err, "roborev fix needs a local daemon")
}

func TestAgentHookEndpointIgnoresRemote(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	ep, err := agentHookEndpoint("")
	require.NoError(t, err)
	assert.NotEqual(t, "daemon-host.example:7474", ep.Address)
}
```

Also add remote-mode tests for:
- `roborev init`: it installs hooks, does not call `registerRepo`, and prints
  the "register this repo on the daemon host" note.
- `roborev remap --quiet`: it exits 0 without contacting any daemon.

Use the existing `init_cmd_test.go` and `remap_test.go` patterns.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/roborev -run 'TestParseRemoteServer|TestRemoteMode|TestEnsureDaemonRemote|TestLocalOnly|TestAgentHookEndpointIgnoresRemote'`
Expected: compile failure.

- [ ] **Step 3: Implement**

`remote_mode.go`:

```go
package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
)

// remoteEndpoint is set when the CLI talks to a daemon on another machine,
// through [remote] server or a non-loopback --server http://host:port.
var remoteEndpoint *daemon.DaemonEndpoint

var probeRemoteDaemon = daemon.ProbeRemoteDaemonPing

func isRemoteMode() bool { return remoteEndpoint != nil }

// parseRemoteServer parses an http://host:port remote daemon URL. Loopback
// hosts return ok=false so they keep today's local behavior.
func parseRemoteServer(raw string) (daemon.DaemonEndpoint, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return daemon.DaemonEndpoint{}, false,
			fmt.Errorf("remote server %q must look like http://host:port", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return daemon.DaemonEndpoint{}, false, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return daemon.DaemonEndpoint{}, false, nil
	}
	if u.Port() == "" {
		return daemon.DaemonEndpoint{}, false, fmt.Errorf("remote server %q needs a port", raw)
	}
	return daemon.DaemonEndpoint{Network: "tcp", Address: u.Host}, true, nil
}

// resolveRemoteEndpoint picks remote mode from --server, or from
// [remote] server when --server is unset.
func resolveRemoteEndpoint() error {
	remoteEndpoint = nil
	raw := serverAddr
	if raw == "" {
		configured, err := config.LoadRemoteServer()
		if err != nil {
			return err
		}
		if configured == "" {
			return nil
		}
		raw = configured
	} else if !strings.HasPrefix(raw, "http://") {
		return nil
	}
	ep, remote, err := parseRemoteServer(raw)
	if err != nil {
		return err
	}
	if remote {
		remoteEndpoint = &ep
	}
	return nil
}

func requireLocalDaemon(command string) error {
	if remoteEndpoint == nil {
		return nil
	}
	return fmt.Errorf("%s needs a local daemon; the remote daemon at http://%s cannot run it",
		command, remoteEndpoint.Address)
}

// ensureLocalDaemon is ensureDaemon for commands that only work against a
// daemon on this machine.
func ensureLocalDaemon(command string) error {
	if err := requireLocalDaemon(command); err != nil {
		return err
	}
	return ensureDaemon()
}

func ensureRemoteDaemon() error {
	if _, err := probeRemoteDaemon(*remoteEndpoint, 2*time.Second); err != nil {
		return fmt.Errorf("remote daemon at http://%s is not reachable: %w", remoteEndpoint.Address, err)
	}
	return nil
}
```

`internal/daemon/runtime.go`: `ProbeDaemonPing` rejects non-loopback TCP
addresses, so a remote probe cannot use it. Split it:

```go
// ProbeDaemonPing validates a local daemon endpoint ... (keep the existing doc)
func ProbeDaemonPing(ep DaemonEndpoint, timeout time.Duration) (*PingInfo, error) {
	if ep.Address == "" {
		return nil, fmt.Errorf("empty daemon address")
	}
	if !ep.IsUnix() && !isLoopbackAddr(ep.Address) {
		return nil, fmt.Errorf("non-loopback daemon address: %s", ep.Address)
	}
	return probePing(ep, timeout)
}

// ProbeRemoteDaemonPing pings a daemon on another machine. The remote
// listener authenticates the caller, so no loopback check applies.
func ProbeRemoteDaemonPing(ep DaemonEndpoint, timeout time.Duration) (*PingInfo, error) {
	if ep.Address == "" || ep.IsUnix() {
		return nil, fmt.Errorf("remote daemon address must be host:port, got %q", ep.Address)
	}
	return probePing(ep, timeout)
}
```

`probePing` holds the rest of today's `ProbeDaemonPing` body unchanged (the
request, the status check, the decode, and the `OK` and service checks). Add
`TestProbeRemoteDaemonPing` in `internal/daemon`, with two cases:
- An `httptest` server whose `/api/ping` returns
  `{"ok":true,"service":"roborev","version":"x"}`: the probe succeeds.
- A Unix endpoint: the probe returns an error.

Check the exact service name against `daemonServiceName`.

`daemon_lifecycle.go`:
- `validateServerFlag`: first run `if err := resolveRemoteEndpoint(); err != nil { return fmt.Errorf("invalid remote server: %w", err) }`.
  Then `if serverAddr == "" || isRemoteMode() { return nil }`, then the
  existing parse.
- Rename the current `getDaemonEndpoint` body to `localDaemonEndpoint()`. The
  new `getDaemonEndpoint` returns `*remoteEndpoint` when it is set, otherwise
  `localDaemonEndpoint()`.
- The first statement of `ensureDaemon`:
  `if remoteEndpoint != nil { return ensureRemoteDaemon() }`.

`agent_hook_client.go`: replace `getDaemonEndpoint()` with
`localDaemonEndpoint()`.

Local-only call sites. Replace `ensureDaemon()` with
`ensureLocalDaemon("<command>")` at:
- fix.go:523, 589, 892, 1234 (`"roborev fix"`)
- refine.go:400, 812, 944 (`"roborev refine"`)
- run.go:238 (`"roborev run"`)
- analyze.go:300 (`"roborev analyze"`)
- compact.go:305 (`"roborev compact"`)
- sync.go:134 (`"roborev sync"`)
- insights.go:130 (`"roborev insights"`)
- snooze.go:62 (`"roborev snooze"`)
- pause.go:50 (`"roborev pause"`)
- export.go:95, export_ci.go:66, export_ci_cost.go:58 (`"roborev export"`)
- daemon_cmd.go at both `daemonEnsure` call sites (`"roborev daemon"`)

Every `roborev daemon` subcommand (start, stop, restart, run, and any
other) acts on the local machine's daemon, and the spec makes lifecycle
commands local-only. Add `if err := requireLocalDaemon("roborev daemon <sub>"); err != nil { return err }`
as the first statement of each subcommand's `RunE` in `daemon_cmd.go`. Do
this before any stop, cleanup, or runtime-file access, so remote mode never
stops or restarts a local daemon. Add a test: in remote mode,
`roborev daemon stop` returns the "needs a local daemon" error, and the stop
hook it would call is never invoked. Find the stubbable stop function in
`daemon_cmd.go`.

In `review.go`, before the dirty path gathers the diff:
`if dirty { if err := requireLocalDaemon("roborev review --dirty"); err != nil { return err } }`.

`init_cmd.go`: when `isRemoteMode()`, install hooks as today, then skip both
`ensureDaemon()` and `registerRepo(root)` and print:

```go
fmt.Fprintf(out, "Remote daemon configured (http://%s). Register this repo on the daemon host by running roborev init there.\n", remoteEndpoint.Address)
```

`remap.go`: at the top of `RunE`:

```go
if isRemoteMode() {
	if quiet {
		return nil
	}
	return requireLocalDaemon("roborev remap")
}
```

`tui_cmd.go`: resolve `--addr` before `ensureDaemon`. For
`strings.HasPrefix(addr, "http://")`, call `parseRemoteServer(addr)`; if it
returns `remote`, set `remoteEndpoint = &ep`. Otherwise keep
`daemon.ParseEndpoint(addr)`. Then call `ensureDaemon()`, which now probes
the remote endpoint.

`mcp_cmd.go` `ensureMCPDaemon`: at the start,
`if isRemoteMode() { return ensureRemoteDaemon() }`. Remote mode skips the
exact-version check.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./cmd/roborev -run 'TestParseRemoteServer|TestRemoteMode|TestEnsureDaemonRemote|TestLocalOnly|TestAgentHookEndpointIgnoresRemote|TestInit|TestRemap'`
Expected: PASS. Then run `go test ./cmd/roborev`. The existing mocks set
`serverAddr = ts.URL` (loopback), so they must stay in local mode.

- [ ] **Step 5: Commit**

```bash
git add cmd/roborev
git commit -m "feat(cli): add remote mode that never manages a local daemon"
```

---

### Task 8: Remote enqueue with pack upload for reviews and the post-commit hook

**Files:**
- Create: `cmd/roborev/remote_enqueue.go`
- Modify: `cmd/roborev/review.go` (~346-403), `cmd/roborev/postcommit.go`
  (~181-213)
- Test: `cmd/roborev/remote_enqueue_test.go`

**Interfaces:**
- Consumes: Task 5 `daemon.EnqueueRequest.RepoIdentity`,
  `daemon.MissingCommitsResponse`, `daemon.MissingCommitsCode`,
  `daemon.RemotePackPath`; Task 7 `isRemoteMode`.
- Produces:
  - `func remoteRepoIdentity(root string) (string, error)`
  - `func resolveRemoteGitRef(ctx context.Context, root, ref string) (string, error)`
  - `func remoteEnqueue(ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client, root string, req daemon.EnqueueRequest) (int, []byte, error)`

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestRemoteRepoIdentityRejectsLocalFallback(t *testing.T) {
	repo := newTestGitRepo(t)
	_, err := remoteRepoIdentity(repo.Dir)
	require.ErrorContains(t, err, ".roborev-id")

	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev-id"), []byte("example/project\n"), 0o644))
	id, err := remoteRepoIdentity(repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, "example/project", id)
}

func TestResolveRemoteGitRef(t *testing.T) {
	assert := assert.New(t)
	repo := newTestGitRepo(t)
	first := repo.CommitFile("a.txt", "a", "first")
	second := repo.CommitFile("b.txt", "b", "second")
	ctx := context.Background()

	got, err := resolveRemoteGitRef(ctx, repo.Dir, "HEAD")
	require.NoError(t, err)
	assert.Equal(second, got)

	got, err = resolveRemoteGitRef(ctx, repo.Dir, first+"^..HEAD")
	require.NoError(t, err)
	assert.Equal(first+"^.."+second, got)

	got, err = resolveRemoteGitRef(ctx, repo.Dir, "HEAD~1..HEAD")
	require.NoError(t, err)
	assert.Equal(first+".."+second, got)
}

// TestRemoteEnqueueUploadsFromForkRemote covers the case where the commit
// sits on a client remote-tracking ref for a remote the daemon lacks.
func TestRemoteEnqueueUploadsFromForkRemote(t *testing.T) {
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", upstream)
	seed := newTestGitRepo(t)
	seed.CommitFile("base.txt", "base", "base")
	gitIn(t, seed.Dir, "push", "-q", upstream, "HEAD:refs/heads/main")

	daemonClone := filepath.Join(root, "daemon")
	laptop := filepath.Join(root, "laptop")
	fork := filepath.Join(root, "fork.git")
	gitIn(t, root, "clone", "-q", upstream, daemonClone)
	gitIn(t, root, "clone", "-q", upstream, laptop)
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", fork)
	require.NoError(t, os.WriteFile(filepath.Join(laptop, ".roborev-id"), []byte("example/project\n"), 0o644))
	gitIn(t, laptop, "add", ".roborev-id")
	gitIn(t, laptop, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "on fork")
	gitIn(t, laptop, "remote", "add", "fork", fork)
	gitIn(t, laptop, "push", "-q", "fork", "HEAD:refs/heads/w")
	gitIn(t, laptop, "fetch", "-q", "fork")
	gitIn(t, laptop, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "unpushed")
	target := gitIn(t, laptop, "rev-parse", "HEAD")
	onFork := gitIn(t, laptop, "rev-parse", "HEAD~1")

	var mu sync.Mutex
	var enqueues []daemon.EnqueueRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		enqueues = append(enqueues, req)
		mu.Unlock()
		if gitCatFileOK(daemonClone, target) {
			respondJSON(w, http.StatusCreated, map[string]any{"id": 7})
			return
		}
		haves := strings.Fields(gitIn(t, daemonClone, "for-each-ref", "--format=%(objectname)"))
		respondJSON(w, http.StatusConflict, daemon.MissingCommitsResponse{
			Error: "missing", Code: daemon.MissingCommitsCode, Missing: []string{target}, Have: haves,
		})
	})
	mux.HandleFunc(daemon.RemotePackPath, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "example/project", r.URL.Query().Get("repo_identity"))
		assert.Equal(t, []string{target}, r.URL.Query()["tip"])
		cmd := exec.Command("git", "-C", daemonClone, "index-pack", "--stdin")
		cmd.Stdin = r.Body
		out, err := cmd.CombinedOutput()
		assert.NoError(t, err, string(out))
		gitIn(t, daemonClone, "update-ref", "refs/roborev/uploads/"+target, target)
		respondJSON(w, http.StatusOK, map[string]any{"pinned": []string{target}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	ep := daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")}

	status, body, err := remoteEnqueue(context.Background(), ep, ts.Client(), laptop,
		daemon.EnqueueRequest{GitRef: "HEAD", Branch: "main", Source: "post_commit"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))
	require.Len(t, enqueues, 2)
	assert.Equal(t, "example/project", enqueues[0].RepoIdentity)
	assert.Empty(t, enqueues[0].RepoPath)
	assert.Equal(t, target, enqueues[0].GitRef)
	assert.True(t, gitCatFileOK(daemonClone, onFork), "fork-only ancestor was packed")
}

func gitCatFileOK(dir, sha string) bool {
	return exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}
```

Also add, following the patterns in `review_test.go` and `postcommit_test.go`:
- `roborev review` in remote mode sends `repo_identity`, sends no
  `repo_path`, and sends a full-SHA `git_ref`.
- `roborev review --dirty` in remote mode fails with "needs a local daemon"
  before any request.
- `roborev review --branch=<other>` in remote mode sends the target branch
  name.
- The post-commit hook in remote mode enqueues remotely and logs a failure
  (does not return an error) when the daemon returns `404`.

The mock daemons listen on loopback, which `parseRemoteServer` treats as
local. To put these tests in remote mode, call `withRemoteState(t)` (Task 7),
then set `remoteEndpoint = &daemon.DaemonEndpoint{Network: "tcp", Address: <mock host:port>}`
after the mock daemon is created.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/roborev -run 'TestRemoteRepoIdentity|TestResolveRemoteGitRef|TestRemoteEnqueue'`
Expected: compile failure.

- [ ] **Step 3: Implement `remote_enqueue.go`**

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// remoteRepoIdentity returns the identity a remote daemon matches for the
// checkout at root. A local:// fallback can never match another host.
func remoteRepoIdentity(root string) (string, error) {
	id := config.ResolveRepoIdentity(root, nil)
	if strings.HasPrefix(id, "local://") {
		return "", fmt.Errorf(
			"repo at %s has no remote URL or .roborev-id, so a remote daemon cannot identify it; add a .roborev-id file matching the daemon host's checkout",
			root)
	}
	return id, nil
}

// resolveRemoteGitRef resolves refs to full SHAs locally, since the daemon
// would resolve names in its own clone. It keeps the inclusive START^..END
// form so the daemon's empty-tree fallback still covers a root START.
func resolveRemoteGitRef(ctx context.Context, root, ref string) (string, error) {
	resolve := func(r string) (string, error) {
		sha, err := gitrepo.Resolve(ctx, root, r+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", r, err)
		}
		return sha, nil
	}
	start, end, isRange := strings.Cut(ref, "..")
	if !isRange {
		return resolve(ref)
	}
	inclusive := strings.HasSuffix(start, "^")
	startSHA, err := resolve(strings.TrimSuffix(start, "^"))
	if err != nil {
		return "", err
	}
	endSHA, err := resolve(end)
	if err != nil {
		return "", err
	}
	if inclusive {
		startSHA += "^"
	}
	return startSHA + ".." + endSHA, nil
}

// remoteEnqueue queues a review on a remote daemon. If the daemon lacks
// the commits, it uploads them as a git pack and retries once.
func remoteEnqueue(
	ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client,
	root string, req daemon.EnqueueRequest,
) (int, []byte, error) {
	identity, err := remoteRepoIdentity(root)
	if err != nil {
		return 0, nil, err
	}
	ref := req.GitRef
	if ref == "" {
		ref, req.CommitSHA = req.CommitSHA, ""
	}
	if req.GitRef, err = resolveRemoteGitRef(ctx, root, ref); err != nil {
		return 0, nil, err
	}
	req.RepoPath, req.RepoIdentity = "", identity
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, err
	}
	status, respBody, err := postRemoteEnqueue(ctx, ep, client, body)
	if err != nil || status != http.StatusConflict {
		return status, respBody, err
	}
	var missing daemon.MissingCommitsResponse
	if json.Unmarshal(respBody, &missing) != nil || missing.Code != daemon.MissingCommitsCode {
		return status, respBody, nil
	}
	if err := uploadRemotePack(ctx, ep, client, root, identity, missing.Missing, missing.Have); err != nil {
		return 0, nil, err
	}
	return postRemoteEnqueue(ctx, ep, client, body)
}

func postRemoteEnqueue(ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client, body []byte) (int, []byte, error) {
	resp, err := newDaemonAPI(ep.BaseURL(), client).EnqueueJobRaw(ctx, nil, roborevclient.WithBody(body))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to connect to remote daemon: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

// uploadRemotePack packs the missing commits, leaving out history reachable
// from daemon ref tips that this repo also has.
func uploadRemotePack(
	ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client,
	root, identity string, missing, have []string,
) error {
	var revs strings.Builder
	for _, sha := range missing {
		revs.WriteString(sha + "\n")
	}
	if len(have) > 0 {
		out, _, err := gitcmd.New().Run(ctx, root, strings.NewReader(strings.Join(have, "\n")+"\n"),
			"cat-file", "--batch-check=%(objectname) %(objecttype)")
		if err != nil {
			return fmt.Errorf("check daemon commits locally: %w", err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			if sha, kind, ok := strings.Cut(line, " "); ok && kind == "commit" {
				revs.WriteString("^" + sha + "\n")
			}
		}
	}
	pack, _, err := gitcmd.New().Run(ctx, root, strings.NewReader(revs.String()),
		"pack-objects", "--revs", "--stdout", "-q")
	if err != nil {
		return fmt.Errorf("build pack for remote daemon: %w", err)
	}
	query := url.Values{"repo_identity": {identity}, "tip": missing}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ep.BaseURL()+daemon.RemotePackPath+"?"+query.Encode(), bytes.NewReader(pack))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("upload commits to remote daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload commits to remote daemon: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
```

`review.go`: restructure the send (~363-376) so that both paths yield
`status int, body []byte, err error`:

```go
			ep := getDaemonEndpoint()
			var status int
			var body []byte
			if isRemoteMode() {
				status, body, err = remoteEnqueue(cmd.Context(), ep, ep.HTTPClient(0), root, reqFields)
				if err != nil {
					return err
				}
			} else {
				resp, err := ep.APIClient(10*time.Second).EnqueueJobRaw(context.Background(), nil, roborevclient.WithBody(reqBody))
				if err != nil {
					return fmt.Errorf("failed to connect to daemon: %w", err)
				}
				defer resp.Body.Close()
				status = resp.StatusCode
				body, _ = io.ReadAll(resp.Body)
			}
```

The rest of the response handling reads `status` in place of
`resp.StatusCode`. The remote client has no whole-request timeout. The
daemon may run `git fetch` before it answers, and a large first upload can
take a while. Ctrl-C cancels both through the command context.

`postcommit.go` (~196-213): the hook runs inside `git commit`, so the
whole remote sequence (enqueue, pack build, upload, and retry) shares one
deadline, not one per request:

```go
			if isRemoteMode() {
				hookCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
				status, body, err = remoteEnqueue(hookCtx, ep, hookHTTPClient(timeout), root,
					daemon.EnqueueRequest{GitRef: gitRef, Branch: branchName, Source: "post_commit"})
			}
```

`gitcmd` runs git with the context, so a deadline that fires mid-pack stops
`pack-objects` too. Add a test with a mock daemon whose `/api/enqueue`
blocks until the request context ends. With a short configured hook
timeout, the hook returns within about that timeout, logs a failure, and
leaves the batch unadvanced. A failed upload is logged with `hookLog(root, "fail", ...)` and
leaves the batch unadvanced, so the next commit retries. Keep the existing
`status >= 400` logging and checkpoint logic for both paths.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./cmd/roborev -run 'TestRemote|TestReview|TestPostCommit'`
Expected: PASS. Then run `go test ./cmd/roborev`.

- [ ] **Step 5: Commit**

```bash
git add cmd/roborev
git commit -m "feat(cli): queue reviews on a remote daemon and upload unpushed commits"
```

---

### Task 9: Remote repo filters from the CLI, TUI, and MCP

**Files:**
- Modify: CLI commands that send a local repo path as a `repo` filter (find
  them with `rg -n 'params.(Add|Set)\("repo"|Repo:\s' cmd/roborev --glob '!*_test.go' --glob '!tui/*'`;
  expect list, summary, cost, search, stream, status, wait)
- Modify: `cmd/roborev/tui/tui.go` (~856-866 auto repo filter; `Config`),
  `cmd/roborev/tui/fetch.go` (`tryReconnect` ~418), `cmd/roborev/tui_cmd.go`
- Modify: `cmd/roborev/mcp_cmd.go` (nothing beyond Task 7 unless a test shows
  otherwise)
- Test: `cmd/roborev/remote_filters_test.go`, `cmd/roborev/tui/remote_test.go`

**Interfaces:**
- Consumes: Task 7 `isRemoteMode`, Task 8 `remoteRepoIdentity`.
- Produces: `func repoFilterValue(root string) (string, error)`, which returns
  `root` in local mode and the identity in remote mode;
  `tui.Config.Remote bool`.

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoFilterValue(t *testing.T) {
	withRemoteState(t)
	repo := newTestGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev-id"), []byte("example/project\n"), 0o644))

	got, err := repoFilterValue(repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, repo.Dir, got, "local mode sends the path")

	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	got, err = repoFilterValue(repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, "example/project", got, "remote mode sends the identity")
}
```

Add a `roborev list` test in remote mode. The mock daemon must receive
`repo=example/project` in the `/api/jobs` query.

TUI tests: see the TUI part of Step 3.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/roborev -run TestRepoFilterValue && go test ./cmd/roborev/tui -run Remote`
Expected: compile failure.

- [ ] **Step 3: Implement**

In `remote_mode.go`:

```go
// repoFilterValue is the repo filter to send for a local checkout: its path
// for a local daemon, its identity for a remote one.
func repoFilterValue(root string) (string, error) {
	if !isRemoteMode() {
		return root, nil
	}
	return remoteRepoIdentity(root)
}
```

At each CLI call site found by the `rg` command above that sends a local
checkout path as `repo`, route the value through `repoFilterValue`. Leave
alone values that came from daemon responses (`root_path` from
`/api/repos`); the remote handler passes those through.

TUI:

The TUI filters jobs on the client too: `filter.go:308` hides every job whose
`RepoPath` is not in `activeRepoFilter`. So in remote mode `activeRepoFilter`
must hold daemon root paths, the same values `/api/repos` returns, never
identities. The TUI uses the checkout's identity only to look up the daemon
root path.

- Add `Remote bool` and `RemoteRepoRoot string` to `tui.Config`, with matching
  options that set `remote` and `remoteRepoRoot` fields on the model's
  options.
- In `tui_cmd.go`, in remote mode, resolve the local checkout to its daemon
  root path before starting the TUI. The checkout is the `--repo` value when
  set, otherwise the current directory's repo if there is one.

```go
// remoteRepoRoot returns the daemon-side root path of the registered repo
// whose identity matches the local checkout at root.
func remoteRepoRoot(ctx context.Context, ep daemon.DaemonEndpoint, root string) (string, error) {
	identity, err := remoteRepoIdentity(root)
	if err != nil {
		return "", err
	}
	resp, err := ep.APIClient(10*time.Second).ListReposRaw(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list repos on remote daemon: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Repos []storage.RepoWithCount `json:"repos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode remote repo list: %w", err)
	}
	var matches []string
	for _, repo := range body.Repos {
		if repo.Identity == identity {
			matches = append(matches, repo.RootPath)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("repo %s is not registered on the remote daemon; run roborev init on the daemon host", identity)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("repo %s matches several remote daemon checkouts: %s", identity, strings.Join(matches, ", "))
	}
}
```

  - Explicit `--repo` in remote mode: pass `RepoFilter: <daemon root path>`.
    An error is returned to the user.
  - Auto-filter in remote mode: pass `RemoteRepoRoot: <daemon root path>`.
    If the lookup fails, start unfiltered and do not error: the current
    directory may simply not be a registered repo.
  - Check the generated client's method name for `GET /api/repos`
    (`ListReposRaw` or similar) in `pkg/client/generated`.
- In `tui.go`, when `opt.remote` is set, replace the locally detected
  `cwdRepoRoot` with `opt.remoteRepoRoot` (empty when unresolved) before the
  auto-filter branch. The existing branch then sets
  `activeRepoFilter = []string{cwdRepoRoot}`, which is now a daemon path, and
  `filter.go`'s job matching and auto-filter checks work unchanged.
- `fetch.go` `tryReconnect`: when remote, return
  `reconnectMsg{endpoint: m.endpoint}` without calling
  `daemon.GetAnyRunningDaemon()`. Use the model's actual endpoint field name.
- `tui.go` ~801 skips reading the daemon version from local runtime files in
  remote mode.

TUI tests:
- In `cmd/roborev/tui/remote_test.go`, with the remote options and
  auto-filter on, `activeRepoFilter` equals `[]string{remoteRepoRoot}`.
- A job whose `RepoPath` equals that root stays visible.
- In remote mode, `tryReconnect` returns the configured endpoint.
- In `cmd/roborev`, test `remoteRepoRoot` against a mock `/api/repos` with
  three cases: one match, no match, and two matches.

MCP: the `repo_path` values its tools accept come from `roborev_list_repos`
(daemon `root_path`), which the remote handler passes through. The only MCP
change is Task 7's remote probe. Confirm with a test that `ensureMCPDaemon`
in remote mode probes the remote endpoint and never calls `ensureDaemon`'s
local path.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./cmd/roborev/... `
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/roborev
git commit -m "feat(cli): filter by repo identity against a remote daemon"
```

---

### Task 10: Documentation and design-constraint updates

**Files:**
- Create: `docs/remote-daemon.md` (add it to the nav in `docs/zensical.toml` next to `web-ui.md`)
- Modify: `docs/configuration.md` (the `remote_api.*` and `remote.server`
  keys)
- Modify: `AGENTS.md` and `CLAUDE.md` (the daemon design constraints)

- [ ] **Step 1: Write the docs page**

Cover, in plain language, following the `AGENTS.md` documentation style:
- **What it does:** read reviews from and queue reviews on a daemon on
  another tailnet machine.
- **Daemon host setup:** a `[remote_api]` example with a Tailscale IP, and
  `tailscale_path`.
- **Tailnet policy grant:** copy the spec's example, with `read` and `queue`
  explained.
- **Client setup:** `[remote] server = "http://<host>:7474"`, or
  `--server http://<host>:7474`.
- **Repo registration:** run `roborev init` on the daemon host. Identities
  must match. Use `.roborev-id` when origin URL forms differ.
- **What works remotely, and what needs a local daemon:** the spec's tables.
- **Unpushed commits:** they upload automatically, and pinned refs clear
  after you push.
- **No reverse proxy:** a proxy in front of the remote listener breaks
  identification. Do not use one.
- **Security:** anyone the grant covers can queue agent runs, which spend
  your quota. Hooks fire for remote reviews.

Use synthetic names only: `daemon-host.example-tailnet.ts.net`,
`100.101.102.103`, `user-a@example.com`.

- [ ] **Step 2: Update the design constraints**

In the `AGENTS.md` bullet that starts "Background daemon work must not edit
tracked source files", and the matching `CLAUDE.md` "Design Constraints"
paragraph, add one sentence:

> Remote pack uploads from tailnet callers write git objects and
> `refs/roborev/uploads/<sha>` refs into a registered clone, and remote
> enqueues may run `git fetch` there. Neither touches the working tree, the
> index, or branches.

- [ ] **Step 3: Format and check**

Run: `make markdown && make markdown-ci`
Expected: no diff after formatting; the check passes.

- [ ] **Step 4: Commit**

```bash
git add docs AGENTS.md CLAUDE.md
git commit -m "docs: describe remote daemon access over a tailnet"
```

---

### Task 11: Full verification

- [ ] **Step 1: Run the repository checks**

```bash
go fmt ./...
go vet ./...
go test ./...
make lint-ci   # with golangci-lint 2.13.1 first on PATH
make api-check
```

Expected: all pass, and `go fmt` leaves no diff.

- [ ] **Step 2: Commit any formatting or generated changes**

```bash
git add -A
git commit -m "chore: format and regenerate after remote daemon support"
```
