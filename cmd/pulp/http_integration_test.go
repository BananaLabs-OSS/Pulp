package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestEchoPluginHTTP spawns the pulp binary against the echo plugin,
// sends HTTP requests to both echo routes, and asserts the responses
// round-trip through the full host / plugin / host path.
func TestEchoPluginHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode")
	}

	binary := filepath.Join("..", "..", "pulp.exe")
	if runtime.GOOS != "windows" {
		binary = filepath.Join("..", "..", "pulp")
	}
	wasmPath := filepath.Join("..", "..", "testdata", "echo", "echo.wasm")
	manifestPath := filepath.Join("..", "..", "testdata", "echo", "pulp.plugin.toml")

	if err := ensurePulpBinary(binary); err != nil {
		t.Fatalf("ensure pulp binary: %v", err)
	}
	if err := buildEchoWASM(t, wasmPath); err != nil {
		t.Fatalf("build echo.wasm: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}

	cmd := exec.Command(binary, "-manifest", manifestPath, "-http-port", fmt.Sprintf("%d", port))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := startWithNewProcessGroup(cmd); err != nil {
		t.Fatalf("start pulp: %v", err)
	}
	defer func() {
		_ = sendInterrupt(cmd.Process.Pid)
		_ = cmd.Wait()
	}()

	if err := waitForLog(&stdout, `msg="http server started"`, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		t.Logf("pulp stdout:\n%s", stdout.String())
		t.Fatalf("http server never started: %v", err)
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("GET /echo/:msg", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/echo/hello", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
		}
		if string(body) != "hello" {
			t.Errorf("body = %q, want hello", body)
		}
	})

	t.Run("POST /echo", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/echo", strings.NewReader("world"))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
		}
		if string(body) != "world" {
			t.Errorf("body = %q, want world", body)
		}
	})

	t.Run("unknown route is 404", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/not-a-route", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// buildEchoWASM compiles testdata/echo/main.go to wasip1 reactor mode.
func buildEchoWASM(t *testing.T, out string) error {
	t.Helper()
	dir := filepath.Dir(out)
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", "echo.wasm", ".")
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, envExcept("GOOS", "GOARCH")...),
		"GOOS=wasip1", "GOARCH=wasm")
	if output, err := cmd.CombinedOutput(); err != nil {
		return &buildErr{output: string(output), err: err}
	}
	return nil
}

// ensurePulpBinary always rebuilds the cmd/pulp binary so the integration
// test runs against current source, not whatever happened to be lying
// around from a prior manual build.
func ensurePulpBinary(path string) error {
	repoRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-o", filepath.Base(path), "./cmd/pulp")
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return &buildErr{output: string(output), err: err}
	}
	return nil
}

// freePort asks the OS for an unused TCP port by binding :0 and closing.
// A tiny TOCTOU window exists between close and pulp's rebind, but in
// test environments it is almost always fine.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}
