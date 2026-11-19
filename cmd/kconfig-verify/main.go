// Command kconfig-verify fails the build if any Kconfig symbol requested
// in the given fragment files did not survive into the final .config —
// olddefconfig silently demotes a symbol whose dependencies are unmet, so
// an explicit CONFIG_X=y is only a request. Run it after the final
// `make olddefconfig` in the kernel derivation.
//
// Usage: kconfig-verify <.config> <fragment>...
package main

import (
	"os"

	"github.com/richiejp/vmjail/internal/kconfig"
)

// osExit and kconfigRun are indirection points so main_test.go can verify
// the wiring without exiting the test process. Plain function values — no
// extra statements to cover.
var (
	osExit     = os.Exit
	kconfigRun = kconfig.Run
)

func main() { osExit(kconfigRun(os.Args, os.Stderr)) }
