package host

// Host-shell proof: the projx cell served over the REAL ext-http transport.
// StartCellHTTP builds the cell, starts ext-http on an ephemeral port, loads +
// Inits the cell with transport.http.inbound, and pumps inbound HTTP through the
// step loop. We then fire genuine HTTP requests at the cell — the full
// browser → host socket → cell → response path — and assert the editor works.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjxCell_HTTP_EditorOverExtHTTP(t *testing.T) {
	h := StartCellHTTP(t, CellHarnessConfig{
		SourceDir:    projxCellSourceDir(),
		Name:         "projx",
		Capabilities: []string{"transport.http.inbound"},
	})

	src, err := os.ReadFile(filepath.Join("..", "safe", "safe.go"))
	if err != nil {
		t.Fatalf("read safe.go: %v", err)
	}

	// --- POST /api/mutate: reorder + rename through real HTTP ---
	reqBody, _ := json.Marshal(map[string]any{
		"src": string(src), "old": "ErrExtensionPanic", "new": "PanicError",
		"func_a": "HostFunc", "func_b": "RecoverHost",
	})
	resp, err := http.Post(h.URL+"/api/mutate", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /api/mutate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/mutate: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Lossless       bool   `json:"lossless"`
		Reordered      bool   `json:"reordered"`
		RenamedIdents  int    `json:"renamed_idents"`
		CommentsBefore int    `json:"comments_before"`
		CommentsAfter  int    `json:"comments_after"`
		CodeRefsOld    int    `json:"code_refs_old"`
		Source         string `json:"source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode mutate response: %v", err)
	}
	if !out.Lossless {
		t.Fatalf("HTTP mutate NOT lossless: %+v", out)
	}
	if !out.Reordered || out.RenamedIdents != 3 {
		t.Fatalf("HTTP mutate: reordered=%v renamed=%d (want true/3)", out.Reordered, out.RenamedIdents)
	}
	if out.CommentsBefore != out.CommentsAfter {
		t.Fatalf("HTTP mutate dropped comments: %d -> %d", out.CommentsBefore, out.CommentsAfter)
	}
	if out.CodeRefsOld != 0 {
		t.Fatalf("HTTP mutate left %d code refs to old symbol", out.CodeRefsOld)
	}
	if !strings.Contains(out.Source, "PanicError") {
		t.Fatalf("HTTP mutate: renamed symbol PanicError not present in returned source")
	}
	t.Logf("POST /api/mutate via real ext-http: lossless=%v reordered=%v renamed=%d comments=%d->%d bytes=%d",
		out.Lossless, out.Reordered, out.RenamedIdents, out.CommentsBefore, out.CommentsAfter, len(out.Source))

	// --- POST /api/project: read side ---
	pBody, _ := json.Marshal(map[string]any{"name": "safe.go", "src": string(src), "func": "HostFunc"})
	pResp, err := http.Post(h.URL+"/api/project", "application/json", bytes.NewReader(pBody))
	if err != nil {
		t.Fatalf("POST /api/project: %v", err)
	}
	defer pResp.Body.Close()
	if pResp.StatusCode != 200 {
		t.Fatalf("POST /api/project: want 200, got %d", pResp.StatusCode)
	}
	var pv struct {
		Nodes   int `json:"nodes"`
		Control int `json:"control"`
		Depth   int `json:"depth"`
	}
	if err := json.NewDecoder(pResp.Body).Decode(&pv); err != nil {
		t.Fatalf("decode project response: %v", err)
	}
	if pv.Nodes < 1 {
		t.Fatalf("project returned no nodes: %+v", pv)
	}
	t.Logf("POST /api/project via real ext-http: nodes=%d control=%d depth=%d", pv.Nodes, pv.Control, pv.Depth)

	// --- GET /: the browser editor UI ---
	idx, err := http.Get(h.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer idx.Body.Close()
	if idx.StatusCode != 200 {
		t.Fatalf("GET /: want 200, got %d", idx.StatusCode)
	}
	if ct := idx.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET /: want text/html, got %q", ct)
	}
	t.Logf("GET / -> 200 text/html (editor UI served over the host socket)")
}
