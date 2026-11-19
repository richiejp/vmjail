package banner

import (
	"strings"
	"testing"
)

func TestStringContainsProjectName(t *testing.T) {
	if !strings.Contains(String, "vmjail") {
		t.Fatalf("banner missing project name: %q", String)
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
