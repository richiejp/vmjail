package agent

import (
	"fmt"
	"io"

	"github.com/localai-org/darkaifactory/internal/oops"
)

// writeReport emits a single line of the form "<prefix><body>\n" to
// w. Used by ApplyShare / RunContainer / runWith and friends to
// surface state on the agent's console.
//
// A write error here means the kernel console (virtio-serial) is
// gone — the host already lost our output. We can't usefully
// continue: subsequent prints would hit the same broken sink and
// the host's banner-scanner would never see the rest of our
// output. oops.Must logs and exits with FatalExitCode, which
// surfaces the failure to the caller (Hello returns
// ErrBannerNotSeen on timeout, with the final exit code in the
// captured console buffer).
func writeReport(w io.Writer, prefix, body string) {
	_, err := fmt.Fprintf(w, "%s%s\n", prefix, body)
	oops.Must("agent: write report", err)
}
