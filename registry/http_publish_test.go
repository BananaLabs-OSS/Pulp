package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPPublisherKeepsCredentialOnWriteSide(t *testing.T) {
	payload := []byte("module")
	digest := Sum(payload)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/releases" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	publisher, err := OpenHTTPPublisher(server.URL, "separate-publishing-token-at-least-32-bytes", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), []byte("manifest"), map[Digest][]byte{digest: payload}); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer separate-publishing-token-at-least-32-bytes" {
		t.Fatalf("authorization = %q", authorization)
	}
}

func TestHTTPPublisherRejectsWeakCredentialAndWrongBlob(t *testing.T) {
	if _, err := OpenHTTPPublisher("https://registry.example", "short", nil); err == nil {
		t.Fatal("accepted weak publishing credential")
	}
	publisher, err := OpenHTTPPublisher("https://registry.example", "separate-publishing-token-at-least-32-bytes", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), nil, map[Digest][]byte{Sum([]byte("right")): []byte("wrong")}); err == nil {
		t.Fatal("accepted blob under the wrong digest")
	}
}
