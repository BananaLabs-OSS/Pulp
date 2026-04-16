package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
)

func TestFetcherDo_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Errorf("X-Test header = %q, want yes", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	f := NewFetcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	resp, err := f.Do(context.Background(), abi.HTTPFetchRequest{
		Method:  "GET",
		URL:     srv.URL,
		Headers: map[string]string{"X-Test": "yes"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("Status = %d, want 202", resp.Status)
	}
	if string(resp.Body) != "hello" {
		t.Errorf("Body = %q, want hello", resp.Body)
	}
	if resp.Headers["Content-Type"] != "text/plain" {
		t.Errorf("Content-Type = %q", resp.Headers["Content-Type"])
	}
}

func TestFetcherDo_POSTBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("body = %q, want payload", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	f := NewFetcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	resp, err := f.Do(context.Background(), abi.HTTPFetchRequest{
		Method: "POST",
		URL:    srv.URL,
		Body:   []byte("payload"),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("Status = %d, want 201", resp.Status)
	}
}

func TestFetcherDo_EmptyURL(t *testing.T) {
	f := NewFetcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := f.Do(context.Background(), abi.HTTPFetchRequest{}); err == nil {
		t.Error("expected error for empty URL")
	}
}

func TestFetcherDo_DefaultsToGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET (default)", r.Method)
		}
	}))
	defer srv.Close()

	f := NewFetcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := f.Do(context.Background(), abi.HTTPFetchRequest{URL: srv.URL}); err != nil {
		t.Fatalf("Do: %v", err)
	}
}
