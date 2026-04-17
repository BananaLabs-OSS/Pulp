//go:build windows

package run

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const (
	ctrlBreakEvent uintptr        = 1
	sigBreak       syscall.Signal = 0x15
)

// InterruptSignal returns SIGBREAK — the signal delivered to a Go
// process when a parent calls GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT).
func InterruptSignal() os.Signal { return sigBreak }

// ShutdownSignals enumerates the signals Main watches on Windows:
// Ctrl+C (os.Interrupt), Ctrl+Break (SIGBREAK), and generic SIGTERM.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, sigBreak, syscall.SIGTERM}
}

// StartWithNewProcessGroup spawns cmd with CREATE_NEW_PROCESS_GROUP
// so the parent can later deliver CTRL_BREAK_EVENT to its process
// group without tearing down its own console. Required for the
// integration-test pattern on Windows.
func StartWithNewProcessGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd.Start()
}

// SendInterrupt delivers CTRL_BREAK_EVENT to the process group
// identified by pid. The target must have been started with
// StartWithNewProcessGroup.
func SendInterrupt(pid int) error {
	r, _, err := syscall.NewLazyDLL("kernel32.dll").
		NewProc("GenerateConsoleCtrlEvent").
		Call(ctrlBreakEvent, uintptr(pid))
	if r == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent: %w", err)
	}
	return nil
}
