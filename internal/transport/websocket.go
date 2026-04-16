package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/coder/websocket"
)

// WSServer manages WebSocket connections. It shares the HTTP listener
// with HTTPServer via path registration: when an HTTP request arrives on
// a path the plugin declared via ws_register, HTTPServer delegates the
// upgrade to this server instead of HTTP dispatch.
//
// Events (open, frame, close) are enqueued for the step loop to drain.
// Writes (ws_send, ws_close) are issued from the step goroutine through
// the per-connection channel so they do not block the step loop.
type WSServer struct {
	logger *slog.Logger

	mu     sync.Mutex
	routes map[string]struct{}
	conns  map[uint64]*wsConn
	nextID atomic.Uint64

	events chan []byte
}

type wsConn struct {
	id     uint64
	conn   *websocket.Conn
	cancel context.CancelFunc
}

// NewWSServer constructs a WSServer. Routes and connections start empty.
func NewWSServer(logger *slog.Logger) *WSServer {
	return &WSServer{
		logger: logger,
		routes: map[string]struct{}{},
		conns:  map[uint64]*wsConn{},
		events: make(chan []byte, 256),
	}
}

// RegisterRoute adds a path the server will accept WebSocket upgrades on.
// Path must begin with "/". Re-registering is a no-op.
func (w *WSServer) RegisterRoute(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("ws path %q must begin with /", path)
	}
	w.mu.Lock()
	w.routes[path] = struct{}{}
	w.mu.Unlock()
	w.logger.Info("ws route registered", "path", path)
	return nil
}

// HasRoute reports whether path has been registered as a WebSocket route.
// Used by HTTPServer to decide whether to delegate an upgrade request.
func (w *WSServer) HasRoute(path string) bool {
	w.mu.Lock()
	_, ok := w.routes[path]
	w.mu.Unlock()
	return ok
}

// Upgrade accepts the WebSocket handshake on a registered route, assigns
// a connection ID, enqueues a ws.open event, and starts the read loop.
func (w *WSServer) Upgrade(rw http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		w.logger.Error("ws accept failed", "err", err, "path", r.URL.Path)
		return
	}

	id := w.nextID.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	c := &wsConn{id: id, conn: conn, cancel: cancel}

	w.mu.Lock()
	w.conns[id] = c
	w.mu.Unlock()

	headers := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	query := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			query[k] = vs[0]
		}
	}

	openPayload, err := abi.EncodeWSOpen(abi.WSOpen{
		ConnID:  id,
		Path:    r.URL.Path,
		Query:   query,
		Headers: headers,
	})
	if err == nil {
		w.enqueueEvent(abi.EventWSOpen, openPayload)
	}

	go w.readLoop(ctx, c)
}

// Send writes a frame to the identified connection. Safe to call from
// the step-loop goroutine only; concurrent Sends on the same connection
// are not supported.
func (w *WSServer) Send(ctx context.Context, req abi.WSSendRequest) error {
	w.mu.Lock()
	c, ok := w.conns[req.ConnID]
	w.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such conn id %d", req.ConnID)
	}
	var mt websocket.MessageType
	switch req.OpCode {
	case abi.WSOpCodeText:
		mt = websocket.MessageText
	case abi.WSOpCodeBinary:
		mt = websocket.MessageBinary
	default:
		return fmt.Errorf("unsupported opcode %d", req.OpCode)
	}
	return c.conn.Write(ctx, mt, req.Payload)
}

// Close issues a close frame and tears down the connection.
func (w *WSServer) Close(req abi.WSCloseRequest) error {
	w.mu.Lock()
	c, ok := w.conns[req.ConnID]
	if ok {
		delete(w.conns, req.ConnID)
	}
	w.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such conn id %d", req.ConnID)
	}
	code := websocket.StatusNormalClosure
	if req.Code != 0 {
		code = websocket.StatusCode(req.Code)
	}
	err := c.conn.Close(code, req.Reason)
	c.cancel()
	return err
}

// PopEvent returns the next encoded StepEvent without blocking. The returned
// bytes are already wrapped — the step loop hands them straight to pulp_step.
func (w *WSServer) PopEvent() ([]byte, bool) {
	select {
	case data := <-w.events:
		return data, true
	default:
		return nil, false
	}
}

// Stop tears down all active connections. Called on host shutdown.
func (w *WSServer) Stop() {
	w.mu.Lock()
	conns := make([]*wsConn, 0, len(w.conns))
	for _, c := range w.conns {
		conns = append(conns, c)
	}
	w.conns = map[uint64]*wsConn{}
	w.mu.Unlock()

	for _, c := range conns {
		_ = c.conn.Close(websocket.StatusGoingAway, "host shutting down")
		c.cancel()
	}
}

func (w *WSServer) readLoop(ctx context.Context, c *wsConn) {
	defer func() {
		w.mu.Lock()
		_, ok := w.conns[c.id]
		if ok {
			delete(w.conns, c.id)
		}
		w.mu.Unlock()
		c.cancel()
	}()

	for {
		msgType, data, err := c.conn.Read(ctx)
		if err != nil {
			code := uint16(websocket.CloseStatus(err))
			reason := err.Error()
			if errors.Is(err, context.Canceled) {
				reason = "host canceled"
			}
			closePayload, encErr := abi.EncodeWSClose(abi.WSClose{
				ConnID: c.id,
				Code:   code,
				Reason: reason,
			})
			if encErr == nil {
				w.enqueueEvent(abi.EventWSClose, closePayload)
			}
			return
		}

		var opcode uint8
		switch msgType {
		case websocket.MessageText:
			opcode = abi.WSOpCodeText
		case websocket.MessageBinary:
			opcode = abi.WSOpCodeBinary
		default:
			continue
		}
		framePayload, err := abi.EncodeWSFrame(abi.WSFrame{
			ConnID:  c.id,
			OpCode:  opcode,
			Payload: data,
		})
		if err != nil {
			continue
		}
		w.enqueueEvent(abi.EventWSFrame, framePayload)
	}
}

func (w *WSServer) enqueueEvent(kind string, payload []byte) {
	ev, err := abi.EncodeStepEvent(kind, payload)
	if err != nil {
		w.logger.Error("encode step event", "kind", kind, "err", err)
		return
	}
	select {
	case w.events <- ev:
	default:
		w.logger.Warn("ws event queue full — dropping event", "kind", kind)
	}
}
