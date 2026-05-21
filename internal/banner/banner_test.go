package banner

import (
	"strings"
	"testing"
)

// TestStringContainsSyllablesInOrder confirms a quick read still
// resolves "darkaifactory" left-to-right despite the drifted baseline
// and random gap positions — if a future redesign reorders the
// syllables or breaks a clump across lines, this fails loudly.
func TestStringContainsSyllablesInOrder(t *testing.T) {
	rest := String
	for _, syl := range []string{"da", "rk", "ai", "fac", "tory."} {
		i := strings.Index(rest, syl)
		if i < 0 {
			t.Fatalf("banner missing syllable %q in:\n%s", syl, String)
		}
		rest = rest[i+len(syl):]
	}
}

// TestStringFramedByBlankLines lets callers concatenate the banner with
// surrounding help text without producing double blanks or missing
// separators. Both ends must carry exactly one newline.
func TestStringFramedByBlankLines(t *testing.T) {
	if !strings.HasPrefix(String, "\n") {
		t.Error("banner should start with a newline")
	}
	if !strings.HasSuffix(String, "\n") {
		t.Error("banner should end with a newline")
	}
	if strings.HasPrefix(String, "\n\n") || strings.HasSuffix(String, "\n\n") {
		t.Error("banner should not have double-newline padding")
	}
}

// TestRenderShapeAcrossRange walks render() across enough rng inputs
// to hit every drift combination, and asserts the invariants that
// should hold for every realisation: three lines, syllables clumped
// per line, line 3 anchored to Width.
func TestRenderShapeAcrossRange(t *testing.T) {
	for seed := range 200 {
		out := render(counterIntN(seed))
		lines := strings.Split(strings.Trim(out, "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("seed=%d want 3 lines, got %d: %q", seed, len(lines), out)
		}
		if !strings.Contains(lines[0], "da") || !strings.Contains(lines[0], "rk") {
			t.Errorf("seed=%d line 0 missing da/rk: %q", seed, lines[0])
		}
		if !strings.Contains(lines[1], "ai") {
			t.Errorf("seed=%d line 1 missing ai: %q", seed, lines[1])
		}
		if !strings.Contains(lines[2], "fac") || !strings.Contains(lines[2], "tory.▮") {
			t.Errorf("seed=%d line 2 missing fac/tory.▮: %q", seed, lines[2])
		}
		for i, line := range lines {
			if w := len([]rune(line)); w > Width {
				t.Errorf("seed=%d line %d width %d > Width %d: %q", seed, i, w, Width, line)
			}
		}
		// Line 3 ("...tory.▮") is the cursor anchor — always exactly Width.
		if w := len([]rune(lines[2])); w != Width {
			t.Errorf("seed=%d line 2 width %d, want %d (cursor anchor)", seed, w, Width)
		}
	}
}

// TestRenderVariesAcrossSeeds confirms different rng inputs actually
// produce different banners — otherwise the randomisation is a no-op.
func TestRenderVariesAcrossSeeds(t *testing.T) {
	seen := make(map[string]struct{})
	for seed := range 30 {
		seen[render(counterIntN(seed))] = struct{}{}
	}
	if len(seen) < 5 {
		t.Errorf("render produced only %d distinct banners across 30 seeds; expected more variation", len(seen))
	}
}

// TestRenderDeterministic — same intN must produce same bytes. Guards
// against accidentally introducing hidden state.
func TestRenderDeterministic(t *testing.T) {
	a := render(counterIntN(7))
	b := render(counterIntN(7))
	if a != b {
		t.Errorf("render not deterministic with same intN:\n%q\nvs\n%q", a, b)
	}
}

// counterIntN returns an intN that walks through start, start+1, ...
// taking each value mod n. Covers every cell of render's input space
// as start varies, without depending on rand seed semantics.
func counterIntN(start int) func(int) int {
	i := start
	return func(n int) int {
		v := ((i % n) + n) % n
		i++
		return v
	}
}
