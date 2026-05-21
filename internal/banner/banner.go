// Package banner exposes the ASCII art shown at the top of every daft-*
// CLI's --help output. Centralising it here means a future redesign edits
// exactly one file.
package banner

import (
	"math/rand/v2"
	"strings"
)

// Width is the rightmost column the banner ever occupies. Line 3
// (...tory.▮) is anchored to it on every render so the cursor sits
// at a stable right margin; shorter lines drift left of it.
const Width = 16

// String is the banner. The leading and trailing newlines let callers
// concatenate it into cobra Long text (or a flag.FlagSet usage func)
// without juggling spacing.
//
// Style: lowercase syllables drift across an indent grid, with a single
// block cursor at the end suggesting a live prompt. The gaps between
// (and around) the syllables are randomised at package init, so each
// process invocation shows a slightly different layout — same three
// lines, same syllable clumps on each line, only the gaps shift.
//
//nolint:gochecknoglobals // randomised at process init; callers read it as a normal string
var String = render(rand.IntN)

// render builds the banner string using intN as its only source of
// randomness. Factored out so tests can pass a deterministic intN and
// assert exact bytes.
func render(intN func(n int) int) string {
	// Per-line drift ranges hug the original layout: wide enough that
	// the eye sees the change between runs, narrow enough that the
	// syllables stay clumped on their lines as before.
	a := intN(5) + 2  // 2..6  line-1 leading (orig 4)
	m1 := intN(3) + 1 // 1..3  line-1 gap     (orig 2)
	b := intN(5) + 5  // 5..9  line-2 leading (orig 7)
	c := intN(3) + 2  // 2..4  line-3 leading (orig 3)
	m2 := 7 - c       // 3..5  line-3 gap; keeps line-3 total at Width

	var sb strings.Builder
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat(" ", a))
	sb.WriteString("da")
	sb.WriteString(strings.Repeat(" ", m1))
	sb.WriteString("rk\n")
	sb.WriteString(strings.Repeat(" ", b))
	sb.WriteString("ai\n")
	sb.WriteString(strings.Repeat(" ", c))
	sb.WriteString("fac")
	sb.WriteString(strings.Repeat(" ", m2))
	sb.WriteString("tory.▮\n")
	return sb.String()
}
