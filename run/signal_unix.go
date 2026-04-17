//go:build !windows

package run

import (
	"os"
	"os/exec"
	"syscall"
)

// InterruptSignal is the signal delivered by tests and the default
// stop path.
func InterruptSignal() os.Signal { return syscall.SIGINT }

// ShutdownSignals enumerates the signals Main watches to trigger a
// graceful shutdown. On Unix that is SIGINT (Ctrl+C) and SIGTERM.
func ShutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// StartWithNewProcessGroup is a no-op on Unix — the default Start
// behavior is sufficient for tests to interrupt their children.
func StartWithNewProcessGroup(cmd *exec.Cmd) error { return cmd.Start() }

// SendInterrupt delivers SIGINT to pid.
func SendInterrupt(pid int) error {
	return syscall.Kill(pid, syscall.SIGINT)
}
