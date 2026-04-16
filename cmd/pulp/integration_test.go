package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func osEnviron() []string { return os.Environ() }

// TestHeartbeatLifecycle spawns the compiled pulp binary against the
// heartbeat test plugin, lets it run for a bit, sends an interrupt, and
// verifies that the full lifecycle logged correctly.
//
// This is the end-to-end v0.1 verification. If this test passes, the
// runtime loads a WASM module, calls init, calls step in a loop, catches
// an interrupt, calls shutdown, and exits clean.
func TestHeartbeatLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode")
	}

	binary := filepath.Join("..", "..", "pulp.exe")
	if runtime.GOOS != "windows" {
		binary = filepath.Join("..", "..", "pulp")
	}
	plugin := filepath.Join("..", "..", "testdata", "heartbeat", "heartbeat.wasm")

	if err := buildHeartbeatWASM(t, plugin); err != nil {
		t.Fatalf("build heartbeat.wasm: %v", err)
	}

	cmd := exec.Command(binary, "-plugin", plugin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := startWithNewProcessGroup(cmd); err != nil {
		t.Fatalf("start pulp: %v", err)
	}

	// Let it run a bit.
	time.Sleep(500 * time.Millisecond)

	if err := sendInterrupt(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("sendInterrupt: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("pulp stdout:\n%s", stdout.String())
			t.Logf("pulp stderr:\n%s", stderr.String())
			t.Fatalf("pulp did not exit cleanly: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Logf("pulp stdout:\n%s", stdout.String())
		t.Logf("pulp stderr:\n%s", stderr.String())
		t.Fatal("pulp did not exit within 3s of interrupt")
	}

	out := stdout.String()

	for _, want := range []string{
		`msg="pulp boot"`,
		`msg="init complete"`,
		`msg="step heartbeat"`,
		`msg="signal received"`,
		`msg="shutdown complete"`,
		`msg="pulp exit clean"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing expected log line %q\nfull output:\n%s", want, out)
		}
	}
}

// buildHeartbeatWASM compiles testdata/heartbeat/main.go to a wasip1 reactor
// module so the integration test is hermetic. Skips rebuild if the output
// already exists and is newer than the source.
func buildHeartbeatWASM(t *testing.T, out string) error {
	t.Helper()
	dir := filepath.Dir(out)
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", "heartbeat.wasm", ".")
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, envExcept("GOOS", "GOARCH")...),
		"GOOS=wasip1", "GOARCH=wasm")
	if output, err := cmd.CombinedOutput(); err != nil {
		return &buildErr{output: string(output), err: err}
	}
	return nil
}

type buildErr struct {
	output string
	err    error
}

func (e *buildErr) Error() string {
	return e.err.Error() + "\n" + e.output
}

// envExcept returns the current process's environment with the listed keys
// removed, so the test can override GOOS/GOARCH without double-setting them.
func envExcept(keys ...string) []string {
	removed := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		removed[k] = struct{}{}
	}
	var out []string
	for _, kv := range osEnviron() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, skip := removed[kv[:eq]]; skip {
			continue
		}
		out = append(out, kv)
	}
	return out
}

