package run

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	_ "github.com/BananaLabs-OSS/Pulp-ext-entropy"
	_ "github.com/BananaLabs-OSS/Pulp-ext-http"
	_ "github.com/BananaLabs-OSS/Pulp-ext-jwt"
	_ "github.com/BananaLabs-OSS/Pulp-ext-oauth"
	_ "github.com/BananaLabs-OSS/Pulp-ext-workers"
)

type composedEmailVerificationOTP struct {
	Email string `msgpack:"email"`
	Code  string `msgpack:"code"`
	Type  string `msgpack:"type"`
}

type composedIdentitySnapshot struct {
	OTPs map[string]composedEmailVerificationOTP `msgpack:"otps"`
}

// TestBananauthComposedEmailHTTPRoute starts the real composed Bananauth
// application with its test-only manifest and reaches the public passwordless
// endpoint over the same Pulp HTTP capability a deployment uses.  It proves
// the route cannot be accidentally supplied by the legacy identity shell.
func TestBananauthComposedEmailHTTPRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping composed Bananauth HTTP integration test in short mode")
	}
	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(workspace, "Bananauth", "application", "testdata", "bananauth-composed-email-smoke.pulp.app.toml")
	if _, err := os.Stat(appPath); err != nil {
		t.Fatalf("test-only composed Bananauth manifest: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	port := multiHostHTTPReservePort(t)
	var logs bytes.Buffer
	runtime, err := NewDirectApplicationRuntime(appPath, DirectApplicationOptions{
		StorageRoot: t.TempDir(),
		HTTPPort:    port,
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("new composed Bananauth runtime: %v", err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("start composed Bananauth runtime: %v\nlogs:\n%s", err, logs.String())
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1:"+port+"/auth/email-verification",
		bytes.NewBufferString(`{"email":"smoke@example.com"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("call composed email-verification route: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("composed email-verification status = %d body=%s, want 200", response.StatusCode, body)
	}
}

// TestSessionsUsesComposedBananauthEmailTransport exercises the real Sessions
// API handler against a live Pulp-hosted Bananauth composition. The only
// white-box step is reading the test runtime's durable OTP snapshot after the
// public issue endpoint generated it; this avoids adding a test code leak to
// the production HTTP contract. The child Vitest test keeps fetch real.
func TestSessionsUsesComposedBananauthEmailTransport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real Sessions-to-Bananauth HTTP integration test in short mode")
	}
	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(workspace, "Bananauth", "application", "testdata", "bananauth-composed-email-smoke.pulp.app.toml")
	storageRoot := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	port := multiHostHTTPReservePort(t)
	var logs bytes.Buffer
	runtime, err := NewDirectApplicationRuntime(appPath, DirectApplicationOptions{
		StorageRoot: storageRoot, HTTPPort: port, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("new composed Bananauth runtime: %v", err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("start composed Bananauth runtime: %v\nlogs:\n%s", err, logs.String())
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	email := "sessions-real-http@example.test"
	issue, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1:"+port+"/auth/email-verification",
		bytes.NewBufferString(`{"email":"`+email+`"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	issue.Header.Set("Content-Type", "application/json")
	issued, err := (&http.Client{Timeout: 10 * time.Second}).Do(issue)
	if err != nil {
		t.Fatalf("issue composed verification code: %v", err)
	}
	defer issued.Body.Close()
	if issued.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(issued.Body)
		t.Fatalf("issue composed verification status = %d body=%s, want 200", issued.StatusCode, body)
	}
	code := composedVerificationCode(t, storageRoot, email)

	vitest := filepath.Join(workspace, "Sessions", "node_modules", ".bin", "vitest.cmd")
	if _, err := os.Stat(vitest); err != nil {
		t.Fatalf("Sessions Vitest executable unavailable: %v", err)
	}
	cmd := exec.CommandContext(ctx, vitest, "run", "src/pages/api/login.bananauth-real-http.integration.test.ts")
	cmd.Dir = filepath.Join(workspace, "Sessions")
	cmd.Env = append(os.Environ(),
		"BANAUTH_REAL_HTTP_ORIGIN=http://127.0.0.1:"+port,
		"BANAUTH_REAL_HTTP_EMAIL="+email,
		"BANAUTH_REAL_HTTP_CODE="+code,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Sessions real Bananauth HTTP integration failed: %v\n%s\nPulp logs:\n%s", err, output, logs.String())
	}
}

func composedVerificationCode(t *testing.T, storageRoot, email string) string {
	t.Helper()
	path := filepath.Join(storageRoot, "apps", "bananauth-composed-email-smoke", "default", "cells", "auth-identity", "primary", "data.db")
	if _, err := os.Stat(path); err != nil {
		var files []string
		_ = filepath.WalkDir(storageRoot, func(found string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && !entry.IsDir() {
				files = append(files, strings.TrimPrefix(found, storageRoot+string(os.PathSeparator)))
			}
			return nil
		})
		t.Fatalf("composed identity store %q is unavailable: %v; storage files=%v", path, err, files)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open composed identity store: %v", err)
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT snapshot FROM auth_identity_commands ORDER BY revision DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("read composed identity snapshot: %v", err)
	}
	var snapshot composedIdentitySnapshot
	if err := msgpack.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode composed identity snapshot: %v", err)
	}
	for _, otp := range snapshot.OTPs {
		if otp.Email == email && otp.Type == "sessions_email_verification" && otp.Code != "" {
			return otp.Code
		}
	}
	t.Fatalf("composed identity snapshot contains no issued verification code for %q", email)
	return ""
}

// TestBananauthComposedOAuthUsesHostProvider verifies the actual public
// composed callback path. The fake Discord server sees the host-held client
// secret, while the guest receives only a verified identity and persists it
// through auth-identity before creating an auth-session.
func TestBananauthComposedOAuthUsesHostProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping composed Bananauth OAuth integration test in short mode")
	}
	fakeDiscord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse fake token request: %v", err)
			}
			if r.Form.Get("client_secret") != "host-only-test-secret" {
				t.Fatalf("host did not supply configured secret")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fake-provider-token"}`))
		case "/api/users/@me":
			if r.Header.Get("Authorization") != "Bearer fake-provider-token" {
				t.Fatalf("fake user request did not receive exchanged token")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"discord-user-42","email":"member@example.test"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeDiscord.Close()
	t.Setenv("PULP_OAUTH_DISCORD_CLIENT_ID", "host-only-test-id")
	t.Setenv("PULP_OAUTH_DISCORD_CLIENT_SECRET", "host-only-test-secret")
	t.Setenv("PULP_OAUTH_DISCORD_REDIRECT_URL", "https://bananauth.example.test/auth/oauth/discord/callback")
	t.Setenv("PULP_OAUTH_DISCORD_API_BASE_URL", fakeDiscord.URL)
	t.Setenv("PULP_JWT_HS256_SECRET", "test-only-host-jwt-secret-with-at-least-32-bytes")

	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(workspace, "Bananauth", "application", "testdata", "bananauth-composed-email-smoke.pulp.app.toml")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	port := multiHostHTTPReservePort(t)
	var logs bytes.Buffer
	runtime, err := NewDirectApplicationRuntime(appPath, DirectApplicationOptions{
		StorageRoot: t.TempDir(), HTTPPort: port, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("new composed Bananauth OAuth runtime: %v", err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("start composed Bananauth OAuth runtime: %v\nlogs:\n%s", err, logs.String())
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	begin, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/auth/oauth/discord", nil)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := client.Do(begin)
	if err != nil {
		t.Fatal(err)
	}
	defer redirect.Body.Close()
	if redirect.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("OAuth authorize status = %d, want %d", redirect.StatusCode, http.StatusTemporaryRedirect)
	}
	location, err := url.Parse(redirect.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse OAuth authorization location: %v", err)
	}
	if location.Query().Get("client_id") != "host-only-test-id" || location.Query().Get("client_secret") != "" {
		t.Fatalf("guest-visible authorization URL was not host-owned: %s", location)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("OAuth authorization URL did not include state")
	}
	callbackURL := "http://127.0.0.1:" + port + "/auth/oauth/discord/callback?code=valid-code&state=" + url.QueryEscape(state)
	callback, err := client.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	defer callback.Body.Close()
	body, _ := io.ReadAll(callback.Body)
	if callback.StatusCode != http.StatusCreated {
		t.Fatalf("OAuth callback status = %d body=%s, want 201", callback.StatusCode, body)
	}
	var created struct {
		AccountID   string `json:"account_id"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.AccountID == "" || created.AccessToken == "" {
		t.Fatalf("OAuth callback did not return a session account: body=%s err=%v", body, err)
	}

	// An OAuth-created account must be able to attach a native credential via
	// the composed auth-identity owner while retaining the same account ID. The
	// protected endpoint also proves the OAuth session is verified by the
	// composed auth-session owner, rather than a legacy local session map.
	attach, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/auth/password/attach",
		bytes.NewBufferString(`{"email":"member@example.test","username":"member","password":"correct-horse-battery-staple"}`))
	if err != nil {
		t.Fatal(err)
	}
	attach.Header.Set("Content-Type", "application/json")
	attach.Header.Set("Authorization", "Bearer "+created.AccessToken)
	attached, err := client.Do(attach)
	if err != nil {
		t.Fatalf("attach native credential: %v", err)
	}
	defer attached.Body.Close()
	attachBody, _ := io.ReadAll(attached.Body)
	if attached.StatusCode != http.StatusCreated {
		t.Fatalf("native attach status = %d body=%s, want 201", attached.StatusCode, attachBody)
	}
	var attachedResult struct {
		AccountID      string `json:"account_id"`
		NativeAttached bool   `json:"native_attached"`
	}
	if err := json.Unmarshal(attachBody, &attachedResult); err != nil || !attachedResult.NativeAttached || attachedResult.AccountID != created.AccountID {
		t.Fatalf("native attach changed OAuth account identity: body=%s err=%v oauth=%q", attachBody, err, created.AccountID)
	}

	login, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/auth/login",
		bytes.NewBufferString(`{"email":"MEMBER@example.test","password":"correct-horse-battery-staple"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Header.Set("Content-Type", "application/json")
	loggedIn, err := client.Do(login)
	if err != nil {
		t.Fatalf("native login after attach: %v", err)
	}
	defer loggedIn.Body.Close()
	loginBody, _ := io.ReadAll(loggedIn.Body)
	if loggedIn.StatusCode != http.StatusOK {
		t.Fatalf("native login status = %d body=%s, want 200", loggedIn.StatusCode, loginBody)
	}
	var nativeLogin struct {
		AccountID   string `json:"account_id"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(loginBody, &nativeLogin); err != nil || nativeLogin.AccountID != created.AccountID || nativeLogin.AccessToken == "" {
		t.Fatalf("native login did not retain OAuth account identity: body=%s err=%v oauth=%q", loginBody, err, created.AccountID)
	}
}
