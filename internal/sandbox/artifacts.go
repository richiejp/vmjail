package sandbox

import (
	"fmt"
	"os"
	"os/exec"
)

// inheritArtifacts translates parent descriptor paths into the child's file
// table. ExtraFiles pins the verified inodes across exec; no cache pathname is
// reopened. Other command arguments and caller-supplied disk paths are unchanged.
func inheritArtifacts(cmd *exec.Cmd, files [4]*os.File) {
	paths := make(map[string]string)
	for _, f := range files {
		if f == nil {
			continue
		}
		parent := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
		child := fmt.Sprintf("/proc/self/fd/%d", 3+len(cmd.ExtraFiles))
		paths[parent] = child
		cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	}
	if path, ok := paths[cmd.Path]; ok {
		cmd.Path = path
	}
	for i, arg := range cmd.Args {
		if path, ok := paths[arg]; ok {
			cmd.Args[i] = path
		}
	}
}
