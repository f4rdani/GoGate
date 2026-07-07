//go:build windows
package tunnel

import "os/exec"

func prepareCmd(cmd *exec.Cmd) {
	// On Windows, child processes are handled normally.
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}
