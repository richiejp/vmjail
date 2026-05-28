// Package kconfig verifies that Kconfig symbols explicitly requested in
// fragment files survived into a built .config.
//
// `make olddefconfig` silently demotes a requested symbol whose
// dependencies aren't met — a CC_HAS_* capability a toolchain bump stops
// advertising, a dependency renamed across a kernel version, another
// symbol it needs flipping off — so an explicit `CONFIG_X=y` is only a
// request, not a guarantee. Verify turns such a silent demotion into an
// error so the build fails loudly instead of shipping a kernel missing a
// feature we asked for (e.g. CFI/FineIBT or FILE_LOCKING).
package kconfig

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"os"
	"sort"
	"strings"
)

// Parse reads a .config or fragment in Kconfig format into a map of symbol
// (the full `CONFIG_*` name) to value. `# CONFIG_X is not set` maps X to
// "n"; `CONFIG_X=y` maps X to "y"; string/int values are stored verbatim
// with any surrounding quotes stripped. Comments and blank lines are
// ignored.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	// .config lines are short, but give a generous cap so a pathological
	// line never silently truncates a symbol.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# CONFIG_") && strings.HasSuffix(line, " is not set"):
			sym := strings.TrimSuffix(strings.TrimPrefix(line, "# "), " is not set")
			out[sym] = "n"
		case strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "CONFIG_"):
			if eq := strings.IndexByte(line, '='); eq > 0 {
				out[line[:eq]] = strings.Trim(line[eq+1:], `"`)
			}
		}
	}
	return out, sc.Err()
}

func parseFile(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 — build-time tool, caller-supplied config paths.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}

// Mismatch is a symbol whose requested value differs from the value that
// ended up in the final .config.
type Mismatch struct {
	Symbol string
	Want   string // requested value ("y", "m", "n", a number, a string)
	Got    string // value in the final .config ("n" if absent)
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s: requested %s, final .config has %s", m.Symbol, show(m.Want), show(m.Got))
}

func show(v string) string {
	if v == "n" {
		return "n (unset)"
	}
	return v
}

// Verify parses configPath and every fragment in fragmentPaths, then
// returns the symbols set by a fragment whose value did not survive into
// configPath. A symbol absent from the final .config counts as "n". When
// two fragments set the same symbol the later one wins, matching the merge
// order passed to merge_config.sh. The result is sorted by symbol name.
func Verify(configPath string, fragmentPaths []string) ([]Mismatch, error) {
	final, err := parseFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", configPath, err)
	}
	want := map[string]string{}
	for _, fp := range fragmentPaths {
		reqs, err := parseFile(fp)
		if err != nil {
			return nil, fmt.Errorf("read fragment %s: %w", fp, err)
		}
		// Later fragment wins, matching the merge_config.sh ordering.
		maps.Copy(want, reqs)
	}
	var ms []Mismatch
	for sym, w := range want {
		got, ok := final[sym]
		if !ok {
			got = "n"
		}
		if got != w {
			ms = append(ms, Mismatch{Symbol: sym, Want: w, Got: got})
		}
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Symbol < ms[j].Symbol })
	return ms, nil
}

// Run is the kconfig-verify command body, split out so it is testable.
// args is os.Args (args[0] is the program name); diagnostics go to stderr;
// the value returned is the process exit code (0 ok, 1 mismatch, 2 usage/IO).
func Run(args []string, stderr io.Writer) int {
	if len(args) < 3 {
		_, _ = fmt.Fprintln(stderr, "usage: kconfig-verify <.config> <fragment>...")
		return 2
	}
	ms, err := Verify(args[1], args[2:])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kconfig-verify: %v\n", err)
		return 2
	}
	for _, m := range ms {
		_, _ = fmt.Fprintln(stderr, "kconfig-verify: "+m.String())
	}
	if len(ms) > 0 {
		_, _ = fmt.Fprintf(stderr, "kconfig-verify: %d requested symbol(s) did not survive olddefconfig\n", len(ms))
		return 1
	}
	return 0
}
