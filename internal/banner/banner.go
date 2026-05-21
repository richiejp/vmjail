// Package banner exposes the ASCII art shown at the top of every daft-*
// CLI's --help output. Centralising it here means a future redesign edits
// exactly one file.
package banner

// String is the banner. The leading and trailing newlines let callers
// concatenate it into cobra Long text (or a flag.FlagSet usage func)
// without juggling spacing.
//
// Style: lowercase syllables drift across an indent grid, with a single
// block cursor at the end suggesting a live prompt. Quiet, terminal-
// native, a hint of phosphor-on-black.
const String = "\n" +
	"    da  rk\n" +
	"       ai\n" +
	"   fac    tory.▮\n"
