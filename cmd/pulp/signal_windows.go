//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Windows Ctrl* event constants and signals. The stdlib exposes
// CREATE_NEW_PROCESS_GROUP on Windows but neither CTRL_BREAK_EVENT nor
// SIGBREAK, so we define them locally to keep dependencies at stdlib-only.
const (
	ctrlBreakEvent uintptr       = 1                 // Win32 CTRL_BREAK_EVENT
	sigBreak       syscall.Signal = 0x15             // POSIX signal 21, delivered to a Go process when it receives CTRL_BREAK_EVENT
)

// interruptSignal returns the signal the host's os/signal.Notify listens
// for on Windows. SIGBREAK is the signal delivered by a parent calling
// GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT) at us, which is the only
// way to send a graceful-interrupt to a Windows child process from Go.
func interruptSignal() os.Signal { return sigBreak }

// shutdownSignals returns the set of signals that should trigger a graceful
// shutdown on Windows: Ctrl+C from a terminal, Ctrl+Break, and a generic
// interrupt sent by another process.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, sigBreak, syscall.SIGTERM}
}

// startWithNewProcessGroup spawns cmd with CREATE_NEW_PROCESS_GROUP so that
// the parent can later deliver a CTRL_BREAK_EVENT to it without tearing down
// its own console.
func startWithNewProcessGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd.Start()
}

// sendInterrupt delivers CTRL_BREAK_EVENT to the process group identified by
// pid. The target must have been started with CREATE_NEW_PROCESS_GROUP.
// Uses only the stdlib — no x/sys dependency.
func sendInterrupt(pid int) error {
	r, _, err := syscall.NewLazyDLL("kernel32.dll").
		NewProc("GenerateConsoleCtrlEvent").
		Call(ctrlBreakEvent, uintptr(pid))
	if r == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent: %w", err)
	}
	return nil
}
