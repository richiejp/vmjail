package main

import (
	"testing"

	"golang.org/x/tools/go/analysis"
)

// TestMainWiring pins that main() calls the multichecker exactly
// once with the bundled analyzer set, and that the return value
// flows through osExit. checkerMain and osExit are swapped so the
// test runs in-process.
func TestMainWiring(t *testing.T) {
	var seen []*analysis.Analyzer
	savedChecker := checkerMain
	checkerMain = func(a ...*analysis.Analyzer) { seen = a }
	t.Cleanup(func() { checkerMain = savedChecker })

	var rc int
	savedExit := osExit
	osExit = func(code int) { rc = code }
	t.Cleanup(func() { osExit = savedExit })

	main()

	if len(seen) != len(analyzers) {
		t.Fatalf("checkerMain saw %d analyzers, want %d", len(seen), len(analyzers))
	}
	wantNames := map[string]bool{
		"assertpure":     true,
		"lintreasons":    true,
		"nogoto":         true,
		"nolinkname":     true,
		"nodoubleptr":    true,
		"chanconst":      true,
		"commandcontext": true,
		"maxbytes":       true,
		"forloopbound":   true,
		"tagcatalog":     true,
		"norecursion":    true,
	}
	for _, a := range seen {
		if !wantNames[a.Name] {
			t.Errorf("unexpected analyzer %q in bundle", a.Name)
		}
		delete(wantNames, a.Name)
	}
	for name := range wantNames {
		t.Errorf("expected analyzer %q missing from bundle", name)
	}
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
}
