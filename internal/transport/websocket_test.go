package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/coder/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

func TestWSServer_RegisterRoute(t *testing.T) {
	w := NewWSServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.RegisterRoute("/chat"); err != nil {
		t.Fatalf("RegisterRoute: %v", err)
	}
	if !w.HasRoute("/chat") {
		t.Error("HasRoute(/chat) = false, want true")
	}
	if w.HasRoute("/other") {
		t.Error("HasRoute(/other) = true, want false")
	}
	if err := w.RegisterRoute("no-slash"); err == nil {
		t.Error("expected error for path missing leading /")
	}
}

func TestWSServer_PopEventEmpty(t *testing.T) {
	w := NewWSServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := w.PopEvent(); ok {
		t.Error("expected no event on empty queue")
	}
}

func TestWSServer_UpgradeEnqueuesOpenEvent(t *testing.T) {
	w := NewWSServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.RegisterRoute("/ws"); err != nil {
		t.Fatalf("RegisterRoute: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(w.Upgrade))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + srv.URL[len("http"):] + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	ev, ok := waitForEvent(t, w, 2*time.Second)
	if !ok {
		t.Fatal("timed out waiting for ws.open")
	}
	if ev.Kind != abi.EventWSOpen {
		t.Errorf("Kind = %q, want %q", ev.Kind, abi.EventWSOpen)
	}
	var open abi.WSOpen
	if err := msgpack.Unmarshal(ev.Payload, &open); err != nil {
		t.Fatalf("decode WSOpen: %v", err)
	}
	if open.ConnID == 0 {
		t.Error("ConnID = 0, want non-zero")
	}
}

func TestWSServer_FrameRoundTrip(t *testing.T) {
	w := NewWSServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.RegisterRoute("/ws"); err != nil {
		t.Fatalf("RegisterRoute: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(w.Upgrade))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	wsURL := "ws" + srv.URL[len("http"):] + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	openEv, ok := waitForEvent(t, w, 2*time.Second)
	if !ok || openEv.Kind != abi.EventWSOpen {
		t.Fatalf("expected ws.open, got %+v ok=%v", openEv, ok)
	}
	var open abi.WSOpen
	_ = msgpack.Unmarshal(openEv.Payload, &open)

	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("client Write: %v", err)
	}

	frameEv, ok := waitForEvent(t, w, 2*time.Second)
	if !ok || frameEv.Kind != abi.EventWSFrame {
		t.Fatalf("expected ws.frame, got %+v ok=%v", frameEv, ok)
	}
	var frame abi.WSFrame
	if err := msgpack.Unmarshal(frameEv.Payload, &frame); err != nil {
		t.Fatalf("decode WSFrame: %v", err)
	}
	if frame.ConnID != open.ConnID {
		t.Errorf("ConnID = %d, want %d", frame.ConnID, open.ConnID)
	}
	if frame.OpCode != abi.WSOpCodeText {
		t.Errorf("OpCode = %d, want %d", frame.OpCode, abi.WSOpCodeText)
	}
	if string(frame.Payload) != "hello" {
		t.Errorf("Payload = %q, want hello", frame.Payload)
	}

	if err := w.Send(ctx, abi.WSSendRequest{
		ConnID:  open.ConnID,
		OpCode:  abi.WSOpCodeText,
		Payload: []byte("back"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mt, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("client Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != "back" {
		t.Errorf("client got mt=%v data=%q", mt, data)
	}
}

func waitForEvent(t *testing.T, w *WSServer, d time.Duration) (abi.StepEvent, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if data, ok := w.PopEvent(); ok {
			ev, err := abi.DecodeStepEvent(data)
			if err != nil {
				t.Fatalf("decode StepEvent: %v", err)
			}
			return ev, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return abi.StepEvent{}, false
}
