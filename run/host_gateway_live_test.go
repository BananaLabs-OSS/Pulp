package run

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestHostGatewayPinsInflightRequestAndRoutesNextToReplacement(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	id := ApplicationIdentity{ApplicationID: "game", InstanceID: "primary"}
	old := &hostGatewayTestRuntime{identity: id, address: "http://old.invalid"}
	next := &hostGatewayTestRuntime{identity: id, address: "http://new.invalid"}
	router := NewAtomicRuntimeRouter(old)
	g, err := NewHostGatewayWithRouters("127.0.0.1:0", []*manifest.RouteBinding{{Path: "/game", Application: "game", Instance: "primary"}}, map[ApplicationIdentity]*AtomicRuntimeRouter{id: router}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "new"
		if request.URL.Host == "old.invalid" {
			close(entered)
			<-release
			body = "old"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	oldResult := make(chan string, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/game/call", nil))
		oldResult <- w.Body.String()
	}()
	<-entered
	quiesced := make(chan error, 1)
	go func() { quiesced <- router.Quiesce(context.Background()) }()
	select {
	case <-quiesced:
		t.Fatal("quiesce completed while request was in flight")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-quiesced; err != nil {
		t.Fatal(err)
	}
	router.Activate(next)
	if got := <-oldResult; got != "old" {
		t.Fatalf("inflight response=%q", got)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/game/call", nil))
	if got := w.Body.String(); got != "new" {
		t.Fatalf("replacement response=%q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
