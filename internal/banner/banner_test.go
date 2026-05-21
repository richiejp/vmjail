package banner

import (
	"strings"
	"testing"
)

// TestStringContainsSyllablesInOrder confirms a quick read still
// resolves "darkaifactory" left-to-right despite the drifted baseline —
// if a future redesign reorders the syllables, this fails loudly.
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

// TestStringIsIndented guards the "indented" half of the design brief.
// Every non-empty line must start with whitespace; a flush-left banner
// would read as a different aesthetic entirely.
func TestStringIsIndented(t *testing.T) {
	for i, line := range strings.Split(strings.Trim(String, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			t.Errorf("line %d not indented: %q", i, line)
		}
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
