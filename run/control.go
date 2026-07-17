// Control socket — graceful per-cell shutdown via a local
// length-prefixed msgpack protocol. Default path: `./pulp.ctl` on unix,
// or wherever PULP_CTL_SOCK points. Permissions 0600 — only the owning
// user can connect. On Windows 1803+ unix sockets work the same way;
// older Windows skips the control socket with a log.
//
// Protocol:
//
//	Request:  <4-byte LE length><msgpack object>
//	Response: <4-byte LE length><msgpack object>
//
// Ops:
//
//	{"op":"status"}                  →  {"cells":[{"name","state","steps"},...]}
//	{"op":"shutdown","cell":"foo"} →  {"ok":true}  or  {"error":"..."}
//	{"op":"shutdown_all"}            →  {"ok":true}
//	{"op":"reload","cell":"foo"}   →  {"ok":true}  or  {"error":"..."}  (live hot-swap)
//
// The server is best-effort — a missing socket path, binding failure,
// or a client crashing mid-request never affects the main host loop.

package run

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	defaultControlSock = "./pulp.ctl"
	maxFrameBytes      = 1 << 20 // 1 MiB — control messages are tiny; cap reads.
)

type ctlRequest struct {
	Op     string `msgpack:"op"`
	Cell string `msgpack:"cell,omitempty"`
}

type ctlStatus struct {
	Name  string `msgpack:"name"`
	State string `msgpack:"state"`
	Steps uint64 `msgpack:"steps"`
}

type ctlResponse struct {
	OK      bool        `msgpack:"ok,omitempty"`
	Error   string      `msgpack:"error,omitempty"`
	Cells []ctlStatus `msgpack:"cells,omitempty"`
}

// controlOps is the narrow view of runtime state the control socket
// needs. The concrete implementation lives in run.Main as a closure
// over the runtimes map.
type controlOps interface {
	status() []ctlStatus
	shutdownCell(name string) error
	shutdownAll() error
	reloadCell(name string) error
}

type controlServer struct {
	ops    controlOps
	logger *slog.Logger
	ln     net.Listener
	addr   string
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{} // active connections; closed on stop()
}

// startControlServer binds the control socket and spawns its accept
// loop. Returns nil if the socket is disabled (PULP_CTL_SOCK="") or
// the bind fails — the host keeps running regardless.
func startControlServer(ops controlOps, logger *slog.Logger) *controlServer {
	addr, disabled := resolveControlAddr()
	if disabled {
		logger.Info("control socket disabled (PULP_CTL_SOCK=\"\")")
		return nil
	}

	// Remove any stale socket from a previous crash. Ignore errors —
	// Listen will surface a clear failure below.
	if _, err := os.Stat(addr); err == nil {
		_ = os.Remove(addr)
	}

	ln, err := net.Listen("unix", addr)
	if err != nil {
		logger.Warn("control socket bind failed; continuing without it",
			"addr", addr, "err", err)
		return nil
	}
	// 0600 perms — only the owning user. Best-effort on Windows.
	_ = os.Chmod(addr, 0o600)

	s := &controlServer{ops: ops, logger: logger, ln: ln, addr: addr, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.acceptLoop()
	logger.Info("control socket listening", "addr", addr)
	return s
}

func resolveControlAddr() (addr string, disabled bool) {
	v, set := os.LookupEnv("PULP_CTL_SOCK")
	if set && v == "" {
		// Explicit opt-out.
		return "", true
	}
	if v != "" {
		return v, false
	}
	return defaultControlSock, false
}

func (s *controlServer) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	// Snapshot and clear active connections before releasing the lock.
	// Closing them unblocks any handle() goroutine blocked on a read,
	// so wg.Wait() below does not hang waiting for an idle client.
	victims := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		victims = append(victims, c)
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()

	_ = s.ln.Close()
	_ = os.Remove(s.addr)
	for _, c := range victims {
		_ = c.Close()
	}
	s.wg.Wait()
}

func (s *controlServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			// Transient accept error — log and keep serving. The listener
			// is still open, so the next Accept will typically succeed.
			// Only a listener Close (handled by the closed branch above)
			// should stop the loop.
			s.logger.Warn("control accept failed; continuing", "err", err)
			continue
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *controlServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Track the connection so stop() can close it and unblock this goroutine.
	s.mu.Lock()
	if s.conns != nil {
		s.conns[conn] = struct{}{}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	// Bound the read so an idle client cannot wedge SIGTERM.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	req, err := readFrame(conn)
	if err != nil {
		s.logger.Warn("control read failed", "err", err)
		return
	}

	var r ctlRequest
	if err := msgpack.Unmarshal(req, &r); err != nil {
		writeResp(conn, ctlResponse{Error: "invalid msgpack: " + err.Error()})
		return
	}

	switch r.Op {
	case "status":
		writeResp(conn, ctlResponse{OK: true, Cells: s.ops.status()})
	case "shutdown":
		if r.Cell == "" {
			writeResp(conn, ctlResponse{Error: "cell name required"})
			return
		}
		if err := s.ops.shutdownCell(r.Cell); err != nil {
			writeResp(conn, ctlResponse{Error: err.Error()})
			return
		}
		writeResp(conn, ctlResponse{OK: true})
	case "shutdown_all":
		if err := s.ops.shutdownAll(); err != nil {
			writeResp(conn, ctlResponse{Error: err.Error()})
			return
		}
		writeResp(conn, ctlResponse{OK: true})
	case "reload":
		if r.Cell == "" {
			writeResp(conn, ctlResponse{Error: "cell name required"})
			return
		}
		if err := s.ops.reloadCell(r.Cell); err != nil {
			writeResp(conn, ctlResponse{Error: err.Error()})
			return
		}
		writeResp(conn, ctlResponse{OK: true})
	default:
		writeResp(conn, ctlResponse{Error: "unknown op: " + r.Op})
	}
}

func readFrame(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n == 0 {
		return nil, errors.New("empty frame")
	}
	if n > maxFrameBytes {
		return nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeResp(w io.Writer, resp ctlResponse) {
	payload, err := msgpack.Marshal(resp)
	if err != nil {
		return
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	_, _ = w.Write(lenBuf[:])
	_, _ = w.Write(payload)
}
