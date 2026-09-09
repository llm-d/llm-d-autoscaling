package tracing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spanNamePattern is the project's span naming convention: a bare snake_case
// verb phrase. The instrumentation scope, not the span name, identifies the
// component, so dotted namespaces such as "wva.reconcile" are rejected.
var spanNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// scopePattern is the instrumentation-scope convention: the short component
// name, optionally followed by the emitting package's path within the repo.
var scopePattern = regexp.MustCompile(`^llm-d-wva(/[a-z0-9]+([_a-z0-9]*))*$`)

func TestSpanNameConstantsFollowConvention(t *testing.T) {
	for _, name := range []string{
		SpanReconcile, SpanCollect, SpanAnalyze, SpanOptimize,
		SpanLimit, SpanEnforce, SpanActuate, SpanPrometheusQuery,
	} {
		assert.Regexp(t, spanNamePattern, name,
			"span names must be bare snake_case; %q is not", name)
	}
}

func TestSpanNamePatternRejectsOldStyleNames(t *testing.T) {
	// Guards the guard: these are the shapes the convention exists to keep out,
	// including the dotted names this project and llm-d-router used before
	// standardizing. If the pattern stops rejecting them it is worthless.
	for _, name := range []string{
		"wva.reconcile",
		"llm_d.pd_proxy.prefill",
		"gateway.request_orchestration",
		"ScorePrefixCache",
		"score-prefix-cache",
		"Score_Prefix_Cache",
		"",
	} {
		assert.NotRegexp(t, spanNamePattern, name,
			"%q must not be accepted as a span name", name)
	}
}

func TestDefaultScopeFollowsConvention(t *testing.T) {
	assert.Regexp(t, scopePattern, instrumentationName)
}

// TestSpanNamesAtCallSitesFollowConvention parses the whole module and checks
// every literal span name passed to a Tracer's Start method.
//
// This is the guard that keeps the convention from eroding: a new call site
// that hardcodes an old-style dotted name (or copies one from another project)
// fails here rather than silently splitting dashboards that group by span name.
func TestSpanNamesAtCallSitesFollowConvention(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	fset := token.NewFileSet()
	checked := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and build output trees are not ours to police.
			switch d.Name() {
			case "vendor", "bin", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Start" {
				return true
			}
			// Only Start calls on a tracer, so unrelated Start methods
			// (tickers, managers, servers) are left alone.
			if !strings.Contains(exprString(sel.X), "Tracer") {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				// A constant or variable: the constant test above covers it.
				return true
			}
			name, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			checked++
			assert.Regexp(t, spanNamePattern, name,
				"%s: span name %q must be bare snake_case (the scope carries the namespace)",
				fset.Position(lit.Pos()), name)
			return true
		})
		return nil
	})
	require.NoError(t, err)

	t.Logf("checked %d literal span names", checked)
}

// exprString renders an expression well enough to recognise a tracer receiver.
func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		return exprString(e.Fun)
	default:
		return ""
	}
}
