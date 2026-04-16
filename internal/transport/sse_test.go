package transport

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
)

func TestSSEServer_RegisterRoute(t *testing.T) {
	s := NewSSEServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.RegisterRoute("/events"); err != nil {
		t.Fatalf("RegisterRoute: %v", err)
	}
	if !s.HasRoute("/events") {
		t.Error("HasRoute(/events) = false, want true")
	}
	if err := s.RegisterRoute("no-slash"); err == nil {
		t.Error("expected error for path missing leading /")
	}
}

func TestSSEServer_EmitRejectsUnknownPath(t *testing.T) {
	s := NewSSEServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Emit(abi.SSEEmitRequest{Path: "/nope", Data: "x"}); err == nil {
		t.Error("expected error emitting on unknown path")
	}
}

func TestFormatSSEFrame(t *testing.T) {
	cases := []struct {
		name string
		in   abi.SSEEmitRequest
		want string
	}{
		{
			name: "data only",
			in:   abi.SSEEmitRequest{Path: "/e", Data: "hello"},
			want: "data: hello\n\n",
		},
		{
			name: "with id and event",
			in:   abi.SSEEmitRequest{Path: "/e", ID: "7", Event: "tick", Data: "hi"},
			want: "id: 7\nevent: tick\ndata: hi\n\n",
		},
		{
			name: "multiline data",
			in:   abi.SSEEmitRequest{Path: "/e", Data: "line1\nline2"},
			want: "data: line1\ndata: line2\n\n",
		},
	}
	for _, tc := range cases {
		got := string(formatSSEFrame(tc.in))
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSSEServer_DeliversToSubscriber(t *testing.T) {
	s := NewSSEServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.RegisterRoute("/events"); err != nil {
		t.Fatalf("RegisterRoute: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(s.Handle))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}

	deadline := time.Now().Add(1 * time.Second)
	for s.Subscribers("/events") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Subscribers("/events") == 0 {
		t.Fatal("subscriber did not register")
	}

	if err := s.Emit(abi.SSEEmitRequest{Path: "/events", Data: "hello"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	var frame strings.Builder
	readDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(readDeadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
		frame.WriteString(line)
		if line == "\n" {
			break
		}
	}
	if !strings.Contains(frame.String(), "data: hello") {
		t.Errorf("frame = %q, want to contain 'data: hello'", frame.String())
	}
}
