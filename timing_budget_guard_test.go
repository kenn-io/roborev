package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pollingBudget struct {
	file   string
	test   string
	method string
	budget time.Duration
}

type pollingBudgetCall struct {
	file   string
	test   string
	method string
	budget time.Duration
	line   int
}

type pollingBudgetAllowance struct {
	reason string
	count  int
}

func (c pollingBudgetCall) String() string {
	return fmt.Sprintf("%s:%d: %s.%s waits %s", c.file, c.line, c.test, c.method, c.budget)
}

func (c pollingBudgetCall) key() pollingBudget {
	return pollingBudget{file: c.file, test: c.test, method: c.method, budget: c.budget}
}

// reviewedPollingBudgets records each retained literal polling call and why it
// still needs a wall-clock budget.
var reviewedPollingBudgets = map[pollingBudget]pollingBudgetAllowance{
	{file: "cmd/roborev/daemon_integration_test.go", test: "TestUpdateDrainCutoverIntegration", method: "Never", budget: 250 * time.Millisecond}: {
		reason: "HTTP status from a separately built daemon process", count: 1,
	},
	{file: "cmd/roborev/daemon_integration_test.go", test: "startIsolatedUpdateDaemon", method: "Eventually", budget: 30 * time.Second}: {
		reason: "update daemon subprocess exit", count: 1,
	},
	{file: "cmd/roborev/mcp_cmd_test.go", test: "TestMCPServeSpeaksProtocolOverStdio", method: "Eventually", budget: 10 * time.Second}: {
		reason: "MCP subprocess stdio delivery", count: 1,
	},
	{file: "cmd/roborev/postcommit_test.go", test: "TestPostCommitBatchSerializesConcurrentHooks", method: "Never", budget: 200 * time.Millisecond}: {
		reason: "postcommit file lock contention and loopback enqueue", count: 1,
	},
	{file: "internal/daemon/browser_handler_test.go", test: "TestBrowserHandlerRemoteReviewMutationsDoNotRunHooks", method: "Eventually", budget: 5 * time.Second}: {
		reason: "worker-backed cancellation and hook completion", count: 2,
	},
	{file: "internal/daemon/browser_server_test.go", test: "TestServerBrowserLifecycleAndRuntimePublication", method: "Eventually", budget: time.Second}: {
		reason: "real browser listener and runtime publication", count: 1,
	},
	{file: "internal/daemon/browser_server_test.go", test: "TestServerBrowserShutdownCancelsActiveEventStream", method: "Eventually", budget: time.Second}: {
		reason: "real browser listener shutdown and stream delivery", count: 2,
	},
	{file: "internal/daemon/runtime_test.go", test: "TestKillDaemonReturnsWhenKnownProcessExitsAndEndpointIsReused", method: "Eventually", budget: time.Second}: {
		reason: "daemon process exit and loopback endpoint reuse", count: 1,
	},
	{file: "internal/daemon/runtime_test.go", test: "TestKillDaemonReturnsWhenKnownProcessExitsAndEndpointIsReused", method: "Eventually", budget: 2 * time.Second}: {
		reason: "daemon process exit and loopback endpoint reuse", count: 1,
	},
	{file: "internal/daemon/server_actions_test.go", test: "TestRunningJobCancellationBroadcastsOnce", method: "Eventually", budget: 5 * time.Second}: {
		reason: "worker-backed job cancellation", count: 2,
	},
	{file: "internal/daemon/server_test.go", test: "startServerAndWaitForRuntime", method: "Eventually", budget: 5 * time.Second}: {
		reason: "real daemon listener startup", count: 1,
	},
	{file: "internal/daemon/server_test.go", test: "stopTestServer", method: "Eventually", budget: 5 * time.Second}: {
		reason: "real daemon listener shutdown", count: 1,
	},
	{file: "internal/daemon/shutdown_test.go", test: "TestStopKeepsBrowserAvailableUntilWorkersFinish", method: "Eventually", budget: time.Second}: {
		reason: "real browser listener and worker shutdown", count: 3,
	},
	{file: "internal/daemon/token_cost_reconciler_test.go", test: "TestTokenCostReconcilerRecoversSessionFromJobLogAtStartup", method: "Eventually", budget: time.Second}: {
		reason: "SQLite-backed token reconciliation", count: 1,
	},
	{file: "internal/daemon/token_cost_reconciler_test.go", test: "TestTokenCostReconcilerAdvancesPastUnavailableCandidate", method: "Eventually", budget: time.Second}: {
		reason: "SQLite-backed token reconciliation", count: 1,
	},
	{file: "internal/daemon/token_cost_reconciler_test.go", test: "TestDelayedTokenCostRetriesAfterImmediateCaptureMiss", method: "Eventually", budget: time.Second}: {
		reason: "SQLite-backed token reconciliation retry", count: 1,
	},
	{file: "internal/daemon/token_cost_reconciler_test.go", test: "TestTokenCostReconcilerDiscoversPersistedCandidateAtStartup", method: "Eventually", budget: time.Second}: {
		reason: "SQLite-backed token reconciliation", count: 1,
	},
	{file: "internal/daemon/update_drain_test.go", test: "TestInterruptPreparationLinearizesWithRetryTransition", method: "Never", budget: 20 * time.Millisecond}: {
		reason: "attemptTransitionsMu contention", count: 1,
	},
	{file: "internal/daemon/update_drain_test.go", test: "TestReleaseClearsInterruptTargetsBeforeOpeningClaimGate", method: "Eventually", budget: 250 * time.Millisecond}: {
		reason: "SQLite write lock contention", count: 1,
	},
	{file: "internal/storage/db_repo_test.go", test: "TestGetOrCreateCommitConcurrentInsert", method: "Eventually", budget: time.Second}: {
		reason: "SQLite concurrent insert contention", count: 1,
	},
}

var externalPollingHelpers = map[string]string{
	"cmd/roborev/daemon_integration_test.go:waitFor":                            "waitFor observes a separately built daemon process",
	"cmd/roborev/update_daemon_test.go:waitForUpdateTestSignal":                 "waitForUpdateTestSignal observes an update daemon",
	"internal/daemon/config_watcher_test.go:requireNever":                       "requireNever observes fsnotify debounce; caller is TestConfigWatcher_InvalidConfigDoesNotCrash",
	"internal/daemon/worker_classify_test.go:waitForEvent":                      "waitForEvent observes worker delivery",
	"internal/daemon/worker_update_interrupt_test.go:waitForUpdateSignal":       "waitForUpdateSignal observes worker interruption",
	"internal/storage/postgres_integration_test.go:waitForSyncWorkerConnection": "waitForSyncWorkerConnection observes PostgreSQL",
	"internal/testutil/testutil.go:WaitForJobStatus":                            "WaitForJobStatus observes SQLite-backed job state",
}

var pollingAssertions = map[string]bool{
	"Eventually":       true,
	"Eventuallyf":      true,
	"EventuallyWithT":  true,
	"EventuallyWithTf": true,
	"Never":            true,
	"Neverf":           true,
}

var timeUnits = map[string]time.Duration{
	"Nanosecond":  time.Nanosecond,
	"Microsecond": time.Microsecond,
	"Millisecond": time.Millisecond,
	"Second":      time.Second,
	"Minute":      time.Minute,
	"Hour":        time.Hour,
}

func TestNoUnreviewedLiteralPollingBudgets(t *testing.T) {
	root := repoRootFromWorkingDir(t)
	found, err := scanPollingBudgetRepository(root)
	require.NoError(t, err)

	helperStale, err := comparePollingHelpers(root, externalPollingHelpers)
	require.NoError(t, err)
	assert.Empty(t, helperStale, "stale external polling helpers: %s", strings.Join(helperStale, ", "))
	unlisted, stale := comparePollingBudgets(found, reviewedPollingBudgets)
	assert.Empty(t, unlisted, "unreviewed literal polling calls:\n%s", strings.Join(unlisted, "\n"))
	assert.Empty(t, stale, "stale reviewed polling calls:\n%s", strings.Join(stale, "\n"))
}

func comparePollingHelpers(root string, allowed map[string]string) ([]string, error) {
	var stale []string
	for key := range allowed {
		separator := strings.LastIndexByte(key, ':')
		if separator < 0 {
			stale = append(stale, key)
			continue
		}
		relPath, funcName := key[:separator], key[separator+1:]
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", relPath, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), relPath, source, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", relPath, err)
		}
		found := false
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Name.Name == funcName {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func scanPollingBudgetRepository(root string) ([]pollingBudgetCall, error) {
	var found []pollingBudgetCall
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && shouldSkipWalkDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", relPath, err)
		}
		calls, err := literalPollingBudgets(filepath.ToSlash(relPath), source)
		if err != nil {
			return err
		}
		found = append(found, calls...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].line < found[j].line
	})
	return found, nil
}

func comparePollingBudgets(found []pollingBudgetCall, allowed map[pollingBudget]pollingBudgetAllowance) (unlisted, stale []string) {
	remaining := map[pollingBudget]int{}
	for budget, allowance := range allowed {
		remaining[budget] = allowance.count
	}
	for _, call := range found {
		if remaining[call.key()] == 0 {
			unlisted = append(unlisted, call.String())
			continue
		}
		remaining[call.key()]--
	}
	for budget, allowance := range allowed {
		if remaining[budget] > 0 {
			stale = append(stale, fmt.Sprintf(
				"%s: %s.%s waits %s (%s, %d missing)",
				budget.file, budget.test, budget.method, budget.budget,
				allowance.reason, remaining[budget],
			))
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	return unlisted, stale
}

// literalPollingBudgets returns every supported literal-duration testify polling call in source.
func literalPollingBudgets(relPath string, source []byte) ([]pollingBudgetCall, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, source, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", relPath, err)
	}

	imports := importAliases(file)
	testifyAliases := map[string]bool{}
	for alias := range imports["github.com/stretchr/testify/assert"] {
		testifyAliases[alias] = true
	}
	for alias := range imports["github.com/stretchr/testify/require"] {
		testifyAliases[alias] = true
	}
	scanner := pollingBudgetScanner{
		fset:           fset,
		relPath:        relPath,
		testifyAliases: testifyAliases,
		timeAliases:    imports["time"],
		receivers:      map[any][]receiverBinding{},
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		scanner.test = fn.Name.Name
		scanner.collectBindings(fn.Body)
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok {
				scanner.check(call)
			}
			return true
		})
	}
	return scanner.found, nil
}

type receiverBinding struct {
	pos        token.Pos
	isReceiver bool
}

type pollingBudgetScanner struct {
	fset           *token.FileSet
	relPath        string
	test           string
	testifyAliases map[string]bool
	timeAliases    map[string]bool
	receivers      map[any][]receiverBinding
	found          []pollingBudgetCall
}

func (s *pollingBudgetScanner) collectBindings(node ast.Node) {
	ast.Inspect(node, func(node ast.Node) bool {
		switch declaration := node.(type) {
		case *ast.AssignStmt:
			for i, rhs := range declaration.Rhs {
				if i >= len(declaration.Lhs) {
					break
				}
				lhs, ok := declaration.Lhs[i].(*ast.Ident)
				if !ok || lhs.Obj == nil {
					continue
				}
				s.recordBinding(lhs.Obj, declaration.Pos(), s.isTestifyNew(rhs))
			}
		case *ast.ValueSpec:
			for i, rhs := range declaration.Values {
				if i >= len(declaration.Names) {
					break
				}
				name := declaration.Names[i]
				if name.Obj == nil {
					continue
				}
				s.recordBinding(name.Obj, declaration.Pos(), s.isTestifyNew(rhs))
			}
		}
		return true
	})
}

func (s *pollingBudgetScanner) recordBinding(object any, pos token.Pos, isReceiver bool) {
	s.receivers[object] = append(s.receivers[object], receiverBinding{
		pos:        pos,
		isReceiver: isReceiver,
	})
}

func (s *pollingBudgetScanner) isTestifyNew(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "New" {
		return false
	}
	qualifier, ok := sel.X.(*ast.Ident)
	return ok && s.isTestifyPackage(qualifier)
}

func (s *pollingBudgetScanner) receiverAt(ident *ast.Ident, pos token.Pos) bool {
	if ident.Obj == nil {
		return false
	}
	current := receiverBinding{}
	for _, binding := range s.receivers[ident.Obj] {
		if binding.pos >= pos {
			break
		}
		current = binding
	}
	return current.isReceiver
}

func (s *pollingBudgetScanner) check(call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !pollingAssertions[sel.Sel.Name] {
		return
	}

	budgetArg := 2
	switch qualifier := sel.X.(type) {
	case *ast.Ident:
		if s.isTestifyPackage(qualifier) {
			break
		}
		if !s.receiverAt(qualifier, call.Pos()) {
			return
		}
		budgetArg = 1
	case *ast.CallExpr:
		if !s.isTestifyNew(qualifier) {
			return
		}
		budgetArg = 1
	default:
		return
	}
	if len(call.Args) <= budgetArg {
		return
	}

	budget, ok := literalDuration(call.Args[budgetArg], s.timeAliases)
	if !ok {
		return
	}
	found := pollingBudgetCall{
		file:   s.relPath,
		test:   s.test,
		method: sel.Sel.Name,
		budget: budget,
		line:   s.fset.Position(call.Pos()).Line,
	}
	s.found = append(s.found, found)
}

func literalDuration(expr ast.Expr, timeAliases map[string]bool) (time.Duration, bool) {
	value, ok := literalDurationValue(expr, timeAliases)
	if !ok {
		return 0, false
	}
	nanos, exact := constant.Int64Val(constant.ToInt(value))
	if !exact {
		return 0, false
	}
	return time.Duration(nanos), true
}

func literalDurationValue(expr ast.Expr, timeAliases map[string]bool) (constant.Value, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.INT && expr.Kind != token.FLOAT {
			return nil, false
		}
		value := constant.MakeFromLiteral(expr.Value, expr.Kind, 0)
		return value, value.Kind() != constant.Unknown
	case *ast.ParenExpr:
		return literalDurationValue(expr.X, timeAliases)
	case *ast.UnaryExpr:
		value, ok := literalDurationValue(expr.X, timeAliases)
		if !ok || (expr.Op != token.ADD && expr.Op != token.SUB) {
			return nil, false
		}
		return constant.UnaryOp(expr.Op, value, 0), true
	case *ast.BinaryExpr:
		left, leftOK := literalDurationValue(expr.X, timeAliases)
		right, rightOK := literalDurationValue(expr.Y, timeAliases)
		if !leftOK || !rightOK {
			return nil, false
		}
		switch expr.Op {
		case token.ADD, token.SUB, token.MUL:
			return constant.BinaryOp(left, expr.Op, right), true
		case token.QUO:
			if constant.Sign(right) == 0 {
				return nil, false
			}
			return constant.BinaryOp(left, token.QUO, right), true
		default:
			return nil, false
		}
	case *ast.SelectorExpr:
		qualifier, ok := expr.X.(*ast.Ident)
		unit, isUnit := timeUnits[expr.Sel.Name]
		if !ok || !timeObjectFromMap(qualifier, timeAliases) || !isUnit {
			return nil, false
		}
		return constant.MakeInt64(int64(unit)), true
	case *ast.CallExpr:
		sel, ok := expr.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Duration" || len(expr.Args) != 1 {
			return nil, false
		}
		qualifier, ok := sel.X.(*ast.Ident)
		if !ok || !timeObjectFromMap(qualifier, timeAliases) {
			return nil, false
		}
		return literalDurationValue(expr.Args[0], timeAliases)
	default:
		return nil, false
	}
}

func (s *pollingBudgetScanner) isTestifyPackage(ident *ast.Ident) bool {
	return ident != nil && ident.Obj == nil && s.testifyAliases[ident.Name]
}

func timeObjectFromMap(ident *ast.Ident, aliases map[string]bool) bool {
	return ident != nil && ident.Obj == nil && aliases[ident.Name]
}

func TestPollingBudgetScannerFindsLiteralCalls(t *testing.T) {
	const imports = `import (
	"testing"
	"time"

	clock "time"
	check "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)`
	source := `package fixture

` + imports + `

type fake struct{}

func TestFixture(t *testing.T) {
	check.Neverf(t, cond, 20*clock.Millisecond, clock.Millisecond, "held")
	require.EventuallyWithT(t, checkValue, time.Second, time.Millisecond)
	require.EventuallyWithTf(t, checkValue, time.Duration(50)*time.Millisecond, time.Millisecond, "held")
	assertions := check.New(t)
	assertions.Never(cond, (time.Second-time.Millisecond), time.Millisecond)
	var check = fake{}
	check.Never(t, cond, 10*time.Millisecond, time.Millisecond)
	if check := (fake{}); true {
		check.Never(t, cond, 11*time.Millisecond, time.Millisecond)
	}
	other.Never(t, cond, 12*time.Millisecond, time.Millisecond)
	const named = 13*time.Millisecond
	assertions.Never(cond, named, time.Millisecond)
}
`
	calls, err := literalPollingBudgets("fixture_test.go", []byte(source))
	require.NoError(t, err)
	got := make([]pollingBudget, len(calls))
	for i, call := range calls {
		got[i] = call.key()
	}
	assert.Equal(t, []pollingBudget{
		{file: "fixture_test.go", test: "TestFixture", method: "Neverf", budget: 20 * time.Millisecond},
		{file: "fixture_test.go", test: "TestFixture", method: "EventuallyWithT", budget: time.Second},
		{file: "fixture_test.go", test: "TestFixture", method: "EventuallyWithTf", budget: 50 * time.Millisecond},
		{file: "fixture_test.go", test: "TestFixture", method: "Never", budget: 999 * time.Millisecond},
	}, got)
	t.Log("20*time.Millisecond -> recognized")
	t.Log("time.Second -> recognized and reviewed")
	t.Log("shadowed var and if names, non-testify Never, and named budget -> ignored")
}

func TestPollingBudgetAllowancesReportUnlistedAndStale(t *testing.T) {
	listed := pollingBudget{file: "a_test.go", test: "TestListed", method: "Never", budget: 250 * time.Millisecond}
	unlisted := pollingBudget{file: "b_test.go", test: "TestUnlisted", method: "Eventually", budget: 20 * time.Millisecond}
	removed := pollingBudget{file: "c_test.go", test: "TestRemoved", method: "Never", budget: 200 * time.Millisecond}

	gotUnlisted, gotStale := comparePollingBudgets(
		[]pollingBudgetCall{
			{file: listed.file, test: listed.test, method: listed.method, budget: listed.budget, line: 10},
			{file: unlisted.file, test: unlisted.test, method: unlisted.method, budget: unlisted.budget, line: 20},
			{file: listed.file, test: listed.test, method: listed.method, budget: listed.budget, line: 30},
		},
		map[pollingBudget]pollingBudgetAllowance{
			listed:  {reason: "separate process", count: 1},
			removed: {reason: "file lock", count: 1},
		},
	)

	assert.Equal(t, []string{"a_test.go:30: TestListed.Never waits 250ms", "b_test.go:20: TestUnlisted.Eventually waits 20ms"}, gotUnlisted)
	assert.Equal(t, []string{"c_test.go: TestRemoved.Never waits 200ms (file lock, 1 missing)"}, gotStale)
	t.Log("duplicate occurrence -> unlisted; removed occurrence -> stale")
}

func TestPollingHelperInventoryReportsStaleAndSourceErrors(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "valid.go"),
		[]byte("package fixture\nfunc TestPresent() {}\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "broken.go"),
		[]byte("package fixture\nfunc Test("),
		0o600,
	))

	stale, err := comparePollingHelpers(root, map[string]string{
		"valid.go:TestPresent": "present",
	})
	require.NoError(t, err)
	assert.Empty(t, stale)

	stale, err = comparePollingHelpers(root, map[string]string{
		"valid.go:TestMissing": "missing",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"valid.go:TestMissing"}, stale)

	_, err = comparePollingHelpers(root, map[string]string{
		"broken.go:TestBroken": "broken source",
	})
	require.Error(t, err)

	_, err = comparePollingHelpers(root, map[string]string{
		"missing.go:TestMissing": "missing source",
	})
	require.ErrorContains(t, err, "read missing.go")
}

func TestPollingBudgetScannerReportsParseErrors(t *testing.T) {
	_, err := literalPollingBudgets("broken_test.go", []byte("package broken\nfunc Test("))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse broken_test.go")
}
