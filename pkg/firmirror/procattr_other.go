//go:build !unix

package firmirror

import "os/exec"

// ownProcessGroup does nothing where there are no process groups.
func ownProcessGroup(*exec.Cmd) {}
