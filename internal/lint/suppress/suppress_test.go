package suppress_test

import (
	"reflect"
	"testing"

	"github.com/richiejp/darkaifactory/internal/lint/suppress"
)

// Shared fixture strings so funlen + goconst stay green and the
// table reads as a list of shapes rather than a list of literals.
const (
	fixtureName   = "foo"
	fixtureReason = "why"
)

type parseCase struct {
	name       string
	raw        string
	wantNames  []string
	wantReason string
	wantOK     bool
}

func parseCases() []parseCase {
	return []parseCase{
		{
			name:   "not a directive",
			raw:    "// just a comment",
			wantOK: false,
		},
		{
			name:   "prefix in middle is not a directive",
			raw:    "// see daft:lint:disable= for context",
			wantOK: false,
		},
		{
			name:       "canonical form",
			raw:        `//daft:lint:disable=foo reason="why"`,
			wantNames:  []string{fixtureName},
			wantReason: fixtureReason,
			wantOK:     true,
		},
		{
			name:       "space after slashes",
			raw:        `// daft:lint:disable=foo reason="why"`,
			wantNames:  []string{fixtureName},
			wantReason: fixtureReason,
			wantOK:     true,
		},
		{
			name:       "multi-analyzer",
			raw:        `//daft:lint:disable=foo,bar reason="why"`,
			wantNames:  []string{fixtureName, "bar"},
			wantReason: fixtureReason,
			wantOK:     true,
		},
		{
			name:      "missing reason",
			raw:       `//daft:lint:disable=foo`,
			wantNames: []string{fixtureName},
			wantOK:    true,
		},
		{
			name:      "empty reason",
			raw:       `//daft:lint:disable=foo reason=""`,
			wantNames: []string{fixtureName},
			wantOK:    true,
		},
		{
			name:      "whitespace-only reason",
			raw:       `//daft:lint:disable=foo reason="   "`,
			wantNames: []string{fixtureName},
			wantOK:    true,
		},
		{
			name:   "bare prefix is malformed",
			raw:    `//daft:lint:disable=`,
			wantOK: true,
		},
		{
			name:       "block comment",
			raw:        `/* daft:lint:disable=foo reason="why" */`,
			wantNames:  []string{fixtureName},
			wantReason: fixtureReason,
			wantOK:     true,
		},
	}
}

func TestParseDirective(t *testing.T) {
	for _, tc := range parseCases() {
		t.Run(tc.name, func(t *testing.T) {
			names, reason, ok := suppress.ParseDirective(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if !reflect.DeepEqual(names, tc.wantNames) {
				t.Errorf("names = %v, want %v", names, tc.wantNames)
			}
		})
	}
}
