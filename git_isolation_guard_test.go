package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type gitIsolationPackage struct {
	dir                 string
	hasIsolatedTestMain bool
	gitEvidence         []string
}

func TestGitUsingTestPackagesUseIsolatedTestMain(t *testing.T) {
	root := repoRootFromWorkingDir(t)
	packages := map[string]*gitIsolationPackage{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".direnv", "bin", "node_modules", "tmp", "vendor":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relDir := filepath.Dir(relPath)
		if relDir == "." {
			relDir = ""
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", relPath, err)
		}

		pkg := packages[relDir]
		if pkg == nil {
			pkg = &gitIsolationPackage{dir: relDir}
			packages[relDir] = pkg
		}

		imports := importAliases(file)
		if testFileCallsIsolatedMain(file, imports["go.kenn.io/roborev/internal/testenv"]) {
			pkg.hasIsolatedTestMain = true
		}
		pkg.gitEvidence = append(pkg.gitEvidence, testFileGitUsage(
			file,
			fset,
			relPath,
			imports["os/exec"],
			imports["go.kenn.io/roborev/internal/testutil"],
		)...)
		return nil
	})
	require.NoError(t, err)

	var missing []string
	for _, pkg := range packages {
		if len(pkg.gitEvidence) == 0 || pkg.hasIsolatedTestMain {
			continue
		}
		dir := pkg.dir
		if dir == "" {
			dir = "."
		}
		missing = append(missing, fmt.Sprintf("%s: %s", dir, pkg.gitEvidence[0]))
	}
	sort.Strings(missing)

	require.Empty(t, missing, "Git-using test packages must call testenv.RunIsolatedMain in TestMain:\n%s", strings.Join(missing, "\n"))
}

func repoRootFromWorkingDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find repo root from %s", dir)
		dir = parent
	}
}

func importAliases(file *ast.File) map[string]map[string]bool {
	aliases := map[string]map[string]bool{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := defaultImportName(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		if aliases[importPath] == nil {
			aliases[importPath] = map[string]bool{}
		}
		aliases[importPath][name] = true
	}
	return aliases
}

func defaultImportName(importPath string) string {
	idx := strings.LastIndex(importPath, "/")
	if idx == -1 {
		return importPath
	}
	return importPath[idx+1:]
}

func testFileCallsIsolatedMain(file *ast.File, testenvAliases map[string]bool) bool {
	if len(testenvAliases) == 0 {
		return false
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RunIsolatedMain" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if ok && testenvAliases[ident.Name] {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func testFileGitUsage(file *ast.File, fset *token.FileSet, relPath string, execAliases, testutilAliases map[string]bool) []string {
	if len(execAliases) == 0 && len(testutilAliases) == 0 {
		return nil
	}

	var evidence []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}

		switch {
		case execAliases[ident.Name] && gitCommandCall(sel.Sel.Name, call.Args):
			evidence = append(evidence, fmt.Sprintf("%s:%d runs git via os/exec", relPath, fset.Position(call.Pos()).Line))
		case testutilAliases[ident.Name] && testutilGitHelper(sel.Sel.Name):
			evidence = append(evidence, fmt.Sprintf("%s:%d uses testutil.%s", relPath, fset.Position(call.Pos()).Line, sel.Sel.Name))
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if ok && testutilGitHelper(ident.Name) {
			evidence = append(evidence, fmt.Sprintf("%s:%d uses %s", relPath, fset.Position(call.Pos()).Line, ident.Name))
		}
		return true
	})
	return evidence
}

func gitCommandCall(name string, args []ast.Expr) bool {
	var commandArg ast.Expr
	switch name {
	case "Command":
		if len(args) == 0 {
			return false
		}
		commandArg = args[0]
	case "CommandContext":
		if len(args) < 2 {
			return false
		}
		commandArg = args[1]
	default:
		return false
	}
	lit, ok := commandArg.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	return err == nil && value == "git"
}

func testutilGitHelper(name string) bool {
	switch name {
	case "GetHeadSHA", "InitTestGitRepo", "InitTestRepo", "NewGitRepo", "NewTestRepo", "NewTestRepoWithCommit":
		return true
	default:
		return false
	}
}
