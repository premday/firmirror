//go:build unix

package firmirror

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs the command in its own process group, so that the Ctrl-C
// meant to let a publish finish does not also kill the signing. The command can
// then outlive a firmirror that is killed outright.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
