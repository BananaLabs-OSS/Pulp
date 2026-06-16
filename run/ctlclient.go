package run

// Control-socket CLIENT. The host (run.Main) is the server; this is the other
// half: `pulp ctl <op> [cell]` connects to the same socket and issues an op.
//
// This is what closes the self-rebuild loop for a cell: a cell holding
// spawn.process can run `<host-exe> ctl reload <its-own-name>` as an ordinary
// child process after it has built a new cell.wasm. The reload then happens
// host-side, out-of-band from the cell's step loop — so the cell never has to
// tear itself down from inside a step (which would be re-entrant).
//
//	pulp ctl status
//	pulp ctl reload <cell>
//	pulp ctl shutdown <cell>
//	pulp ctl shutdown_all

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// RunCtl runs the control-socket client and returns a process exit code.
// args is os.Args after the "ctl" subcommand token.
func RunCtl(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: <pulp> ctl <status|reload|shutdown|shutdown_all> [cell]")
		return 2
	}
	req := ctlRequest{Op: args[0]}
	switch req.Op {
	case "reload", "shutdown":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "op %q requires a cell name\n", req.Op)
			return 2
		}
		req.Cell = args[1]
	case "status", "shutdown_all":
		// no cell
	default:
		fmt.Fprintf(os.Stderr, "unknown op: %s\n", req.Op)
		return 2
	}

	addr, disabled := resolveControlAddr()
	if disabled {
		fmt.Fprintln(os.Stderr, "control socket disabled (PULP_CTL_SOCK=\"\")")
		return 1
	}

	conn, err := net.DialTimeout("unix", addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect %s: %v\n", addr, err)
		return 1
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	payload, err := msgpack.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode request: %v\n", err)
		return 1
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		return 1
	}
	if _, err := conn.Write(payload); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		return 1
	}

	respBytes, err := readFrame(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read response: %v\n", err)
		return 1
	}
	var resp ctlResponse
	if err := msgpack.Unmarshal(respBytes, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "decode response: %v\n", err)
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		return 1
	}
	if req.Op == "status" {
		for _, c := range resp.Cells {
			fmt.Printf("%-24s %-10s steps=%d\n", c.Name, c.State, c.Steps)
		}
		return 0
	}
	fmt.Println("ok")
	return 0
}
