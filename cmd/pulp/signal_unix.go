//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func interruptSignal() os.Signal { return syscall.SIGINT }

func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

func startWithNewProcessGroup(cmd *exec.Cmd) error { return cmd.Start() }

func sendInterrupt(pid int) error {
	return syscall.Kill(pid, syscall.SIGINT)
}
