package kata

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindingReadsKataToml(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"),
		[]byte("version = 1\n[project]\nname = \"myproj\"\n"), 0o644))

	c := NewCLIClient(dir)
	b, err := c.Binding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "myproj", b.Project)
}

func TestBindingWalksUp(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"),
		[]byte("[project]\nname = \"parentproj\"\n"), 0o644))
	child := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(child, 0o755))

	b, err := NewCLIClient(child).Binding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "parentproj", b.Project)
}

func TestBindingNoBinding(t *testing.T) {
	_, err := NewCLIClient(t.TempDir()).Binding(context.Background())
	assert.ErrorIs(t, err, ErrNoBinding)
}

func TestBindingEmptyNameKeepsWalking(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"),
		[]byte("[project]\nname = \"parentproj\"\n"), 0o644))
	child := filepath.Join(root, "child")
	require.NoError(t, os.MkdirAll(child, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(child, ".kata.toml"),
		[]byte("[project]\n"), 0o644)) // present but no name

	b, err := NewCLIClient(child).Binding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "parentproj", b.Project)
}

func TestBindingMalformedTomlStops(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"),
		[]byte("[project]\nname = \"parentproj\"\n"), 0o644))
	child := filepath.Join(root, "child")
	require.NoError(t, os.MkdirAll(child, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(child, ".kata.toml"),
		[]byte("this is not valid toml ===\n"), 0o644))

	b, err := NewCLIClient(child).Binding(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoBinding)
	assert.Empty(t, b.Project)
}

func TestBindingLocalTomlOverrides(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"),
		[]byte("[project]\nname = \"committed\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte("[project]\nname = \"local\"\n"), 0o644))

	b, err := NewCLIClient(dir).Binding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "local", b.Project)
}

func TestBindingLocalServerOnlyFallsThrough(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"),
		[]byte("[project]\nname = \"committed\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte("[server]\nurl = \"http://x\"\n"), 0o644))

	b, err := NewCLIClient(dir).Binding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "committed", b.Project)
}

func TestBindingMalformedLocalTomlStops(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"),
		[]byte("[project]\nname = \"parentproj\"\n"), 0o644))
	child := filepath.Join(root, "child")
	require.NoError(t, os.MkdirAll(child, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(child, ".kata.local.toml"),
		[]byte("this is not valid toml ===\n"), 0o644))

	b, err := NewCLIClient(child).Binding(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoBinding)
	assert.Empty(t, b.Project)
}

// stubClient returns a CLIClient whose exec is replaced by a canned response.
func stubClient(out string, err error) (*CLIClient, *[]string) {
	var gotArgs []string
	c := NewCLIClient("")
	c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		gotArgs = args
		return []byte(out), err
	}
	return c, &gotArgs
}

func TestListParsesEnvelope(t *testing.T) {
	c, args := stubClient(`{"kata_api_version":1,"issues":[
		{"short_id":"abc4","qualified_id":"myproj#abc4","title":"T1","body":"B1","status":"open","labels":["x"]},
		{"short_id":"def5","title":"T2","body":"B2","status":"open"}]}`, nil)

	issues, err := c.List(context.Background(), ListOpts{Status: "open"})
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, "abc4", issues[0].ShortID)
	assert.Equal(t, "myproj#abc4", issues[0].QualifiedID)
	assert.Equal(t, "B1", issues[0].Body)
	assert.Equal(t, []string{"x"}, issues[0].Labels)
	assert.Equal(t, []string{"list", "--json", "--status", "open"}, *args)
}

func TestListDefaultsStatusOpen(t *testing.T) {
	c, args := stubClient(`{"issues":[]}`, nil)
	_, err := c.List(context.Background(), ListOpts{})
	require.NoError(t, err)
	assert.Equal(t, []string{"list", "--json", "--status", "open"}, *args)
}

func TestShowParsesEnvelope(t *testing.T) {
	c, args := stubClient(`{"kata_api_version":1,"issue":
		{"short_id":"abc4","title":"Task","body":"Do the thing","status":"open"},
		"labels":[{"label":"bug"},{"label":"p1"}]}`, nil)

	iss, err := c.Show(context.Background(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, "Task", iss.Title)
	assert.Equal(t, "Do the thing", iss.Body)
	assert.Equal(t, []string{"bug", "p1"}, iss.Labels)
	assert.Equal(t, []string{"show", "abc4", "--json"}, *args)
}

func TestShowNoLabels(t *testing.T) {
	c, _ := stubClient(`{"kata_api_version":1,"issue":
		{"short_id":"abc4","title":"Task","status":"open"}}`, nil)

	iss, err := c.Show(context.Background(), "abc4")
	require.NoError(t, err)
	assert.Empty(t, iss.Labels)
}

func TestCreateParsesEnvelopeAndArgs(t *testing.T) {
	c, args := stubClient(`{"kata_api_version":1,"issue":{"short_id":"zzz9"},"changed":true,"reused":true}`, nil)
	p := 2
	res, err := c.Create(context.Background(), CreateReq{
		Title: "title", Body: "body", Project: "myproj",
		Labels: []string{"roborev", "review-finding"}, Priority: &p,
		IdempotencyKey: "roborev:7:review.completed:sha",
	})
	require.NoError(t, err)
	assert.Equal(t, "zzz9", res.ShortID)
	assert.True(t, res.Reused)
	assert.Equal(t, []string{
		"create", "title", "--json", "--body-stdin",
		"--project", "myproj",
		"--label", "roborev", "--label", "review-finding",
		"--priority", "2",
		"--idempotency-key", "roborev:7:review.completed:sha",
	}, *args)
}

func TestCreateReusedAbsentMeansFalse(t *testing.T) {
	c, _ := stubClient(`{"issue":{"short_id":"zzz9"},"changed":true}`, nil)
	res, err := c.Create(context.Background(), CreateReq{Title: "t"})
	require.NoError(t, err)
	assert.False(t, res.Reused)
}

func TestListPropagatesError(t *testing.T) {
	c, _ := stubClient("", ErrUnavailable)
	_, err := c.List(context.Background(), ListOpts{})
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestCreateZeroPriorityEmitted(t *testing.T) {
	c, args := stubClient(`{"issue":{"short_id":"z0"}}`, nil)
	p0 := 0
	_, err := c.Create(context.Background(), CreateReq{Title: "t", Priority: &p0})
	require.NoError(t, err)
	assert.Contains(t, *args, "--priority")
	assert.Contains(t, *args, "0")
}
