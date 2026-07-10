package host

// Pilot: ONE real Evolution gene method (gene.catalog) over the typed
// canonical-ABI sibling path, msgpack fallback intact. Mirrors
// cellharness_witcell_test.go but drives the REAL gene wire contract
// (Fiber/pulp/gene) via the pilot cell testdata/gene-typed.
//
//   - NEW PATH: gene.catalog round-trips a rich RegistrationInfo (name,
//     version, list<sku> with a nested metadata map, list<route-decl>,
//     list<admin-tab>, list<string>) through Cell.CallTyped — the host LIFTS
//     the typed pointer tree while pinned, then pulp_post_return tree-frees it
//     to ZERO leaked pins (probe_alloc_live back to baseline).
//
//   - UNCHANGED FALLBACK: on the SAME cell, gene.admin_fragment still serves
//     over the opaque msgpack Cell.Call path exactly as before. Every gene
//     method except gene.catalog — and every deployed gene that never opts in —
//     rides this unchanged path.
//
// We must NOT import Fiber/pulp/gene here (its //go:wasmimport bodies only
// compile under GOOS=wasip1). The gene wire contract under test is just the two
// function-name strings + the canonical-ABI layout, mirrored below.

import (
	"errors"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	fnCatalog       = "gene.catalog"
	fnAdminFragment = "gene.admin_fragment"
)

// ---- host-side static-layout readers (the LIFT side; wasm32 LE) ----

func gtRdU8(mem api.Memory, a uint32) byte    { v, _ := mem.ReadByte(a); return v }
func gtRdU32(mem api.Memory, a uint32) uint32 { v, _ := mem.ReadUint32Le(a); return v }
func gtRdU64(mem api.Memory, a uint32) uint64 { v, _ := mem.ReadUint64Le(a); return v }

func gtStr(mem api.Memory, ptr, ln uint32) string {
	if ln == 0 {
		return ""
	}
	b, _ := mem.Read(ptr, ln)
	return string(b)
}
func gtStrSlot(mem api.Memory, a uint32) string {
	return gtStr(mem, gtRdU32(mem, a+0), gtRdU32(mem, a+4))
}
func gtListStr(mem api.Memory, a uint32) []string {
	base := gtRdU32(mem, a+0)
	n := gtRdU32(mem, a+4)
	out := make([]string, n)
	for i := uint32(0); i < n; i++ {
		out[i] = gtStrSlot(mem, base+i*8)
	}
	return out
}
func gtKVMap(mem api.Memory, a uint32) map[string]string {
	base := gtRdU32(mem, a+0)
	n := gtRdU32(mem, a+4)
	out := make(map[string]string, n)
	for i := uint32(0); i < n; i++ {
		slot := base + i*16
		out[gtStrSlot(mem, slot)] = gtStrSlot(mem, slot+8)
	}
	return out
}

type gtSKU struct {
	ID, Name, Description string
	PriceCents            int64
	Currency              string
	Metadata              map[string]string
}
type gtRoute struct{ Method, Path string }
type gtAdminTab struct {
	Key, Label, Icon string
	Order            int32
}
type gtInfo struct {
	Name, Version  string
	SKUs           []gtSKU
	Routes         []gtRoute
	AdminTabs      []gtAdminTab
	EmailTemplates []string
	IsErr          bool
	ErrArm         string
}

func gtSKUs(mem api.Memory, a uint32) []gtSKU {
	base := gtRdU32(mem, a+0)
	n := gtRdU32(mem, a+4)
	out := make([]gtSKU, n)
	for i := uint32(0); i < n; i++ {
		slot := base + i*48
		out[i] = gtSKU{
			ID:          gtStrSlot(mem, slot+0),
			Name:        gtStrSlot(mem, slot+8),
			Description: gtStrSlot(mem, slot+16),
			PriceCents:  int64(gtRdU64(mem, slot+24)),
			Currency:    gtStrSlot(mem, slot+32),
			Metadata:    gtKVMap(mem, slot+40),
		}
	}
	return out
}
func gtRoutes(mem api.Memory, a uint32) []gtRoute {
	base := gtRdU32(mem, a+0)
	n := gtRdU32(mem, a+4)
	out := make([]gtRoute, n)
	for i := uint32(0); i < n; i++ {
		slot := base + i*16
		out[i] = gtRoute{Method: gtStrSlot(mem, slot+0), Path: gtStrSlot(mem, slot+8)}
	}
	return out
}
func gtAdminTabs(mem api.Memory, a uint32) []gtAdminTab {
	base := gtRdU32(mem, a+0)
	n := gtRdU32(mem, a+4)
	out := make([]gtAdminTab, n)
	for i := uint32(0); i < n; i++ {
		slot := base + i*28
		out[i] = gtAdminTab{
			Key:   gtStrSlot(mem, slot+0),
			Label: gtStrSlot(mem, slot+8),
			Icon:  gtStrSlot(mem, slot+16),
			Order: int32(gtRdU32(mem, slot+24)),
		}
	}
	return out
}

// gtLift decodes result<registration-info, string> at recPtr (align 4, size 52).
func gtLift(mem api.Memory, recPtr uint32) (gtInfo, error) {
	switch gtRdU8(mem, recPtr+0) {
	case 0:
		return gtInfo{
			Name:           gtStrSlot(mem, recPtr+4),
			Version:        gtStrSlot(mem, recPtr+12),
			SKUs:           gtSKUs(mem, recPtr+20),
			Routes:         gtRoutes(mem, recPtr+28),
			AdminTabs:      gtAdminTabs(mem, recPtr+36),
			EmailTemplates: gtListStr(mem, recPtr+44),
		}, nil
	case 1:
		return gtInfo{IsErr: true, ErrArm: gtStrSlot(mem, recPtr+4)}, nil
	default:
		return gtInfo{}, errors.New("invalid result discriminant")
	}
}

// TestGeneCatalogTyped_NewPath drives the pilot gene cell's gene.catalog over
// CallTyped and asserts a byte-correct lift of the rich RegistrationInfo plus a
// zero-leak tree-free.
func TestGeneCatalogTyped_NewPath(t *testing.T) {
	cell, ctx := loadTestdataCell(t, "../../testdata/gene-typed", "gene-typed")

	if !cell.ExportsPostReturn() {
		t.Fatal("pilot gene cell must export pulp_post_return")
	}

	base := probeAllocLive(t, ctx, cell)

	var got gtInfo
	var liftedLen uint32
	err := cell.CallTyped(ctx, fnCatalog, nil, func(mem api.Memory, respPtr, respLen uint32) error {
		liftedLen = respLen
		r, e := gtLift(mem, respPtr)
		got = r
		return e
	})
	if err != nil {
		t.Fatalf("CallTyped(%s): %v", fnCatalog, err)
	}
	if got.IsErr {
		t.Fatalf("unexpected err arm: %s", got.ErrArm)
	}
	if liftedLen != 52 {
		t.Errorf("respLen = %d, want 52 (result<registration-info,string> size)", liftedLen)
	}
	if got.Name != "pilot" {
		t.Errorf("Name = %q, want %q", got.Name, "pilot")
	}
	if got.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", got.Version, "1.2.3")
	}

	if len(got.SKUs) != 2 {
		t.Fatalf("SKUs = %+v, want 2", got.SKUs)
	}
	sku0 := got.SKUs[0]
	if sku0.ID != "sku-basic" || sku0.Name != "Basic Session" || sku0.Description != "one server" {
		t.Errorf("SKU0 strings = %+v", sku0)
	}
	if sku0.PriceCents != 1400 {
		t.Errorf("SKU0.PriceCents = %d, want 1400", sku0.PriceCents)
	}
	if sku0.Currency != "usd" {
		t.Errorf("SKU0.Currency = %q, want usd", sku0.Currency)
	}
	if sku0.Metadata["tier"] != "basic" || sku0.Metadata["region"] != "us" {
		t.Errorf("SKU0.Metadata = %v, want tier=basic region=us", sku0.Metadata)
	}
	sku1 := got.SKUs[1]
	if sku1.ID != "sku-plus" || sku1.PriceCents != 2900 || sku1.Description != "" {
		t.Errorf("SKU1 = %+v", sku1)
	}
	if len(sku1.Metadata) != 0 {
		t.Errorf("SKU1.Metadata = %v, want empty (nil map lowered as null list)", sku1.Metadata)
	}

	if len(got.Routes) != 2 {
		t.Fatalf("Routes = %+v, want 2", got.Routes)
	}
	if got.Routes[0] != (gtRoute{"GET", "/api/voucher/:id/redeem"}) {
		t.Errorf("Routes[0] = %+v", got.Routes[0])
	}
	if got.Routes[1] != (gtRoute{"POST", "/api/session/:id/deploy"}) {
		t.Errorf("Routes[1] = %+v", got.Routes[1])
	}

	if len(got.AdminTabs) != 1 {
		t.Fatalf("AdminTabs = %+v, want 1", got.AdminTabs)
	}
	if got.AdminTabs[0] != (gtAdminTab{Key: "tiers", Label: "Tier Management", Icon: "layers", Order: 1}) {
		t.Errorf("AdminTabs[0] = %+v", got.AdminTabs[0])
	}

	if want := []string{"purchased", "ready", "expired"}; len(got.EmailTemplates) != 3 ||
		got.EmailTemplates[0] != want[0] || got.EmailTemplates[1] != want[1] || got.EmailTemplates[2] != want[2] {
		t.Errorf("EmailTemplates = %v, want %v", got.EmailTemplates, want)
	}

	// ZERO LEAK: pulp_post_return tree-freed the whole response (top record +
	// every string / list / nested-map sub-buffer), and CallTyped freed its own
	// name/args/out buffers. Pins must be back to baseline.
	after := probeAllocLive(t, ctx, cell)
	if after != base {
		t.Errorf("pin leak: base=%d after=%d (pulp_post_return must tree-free the whole response)", base, after)
	}
}

// TestGeneCatalogTyped_MsgpackFallbackIntact proves that on the SAME pilot cell,
// a DIFFERENT gene method (gene.admin_fragment) still round-trips over the
// unchanged opaque msgpack Cell.Call path — the fallback the graft must not
// disturb.
func TestGeneCatalogTyped_MsgpackFallbackIntact(t *testing.T) {
	cell, ctx := loadTestdataCell(t, "../../testdata/gene-typed", "gene-typed")

	arg, err := msgpack.Marshal("tiers")
	if err != nil {
		t.Fatalf("marshal tab: %v", err)
	}
	out, err := cell.Call(ctx, fnAdminFragment, arg)
	if err != nil {
		t.Fatalf("Call(%s): %v", fnAdminFragment, err)
	}
	var html string
	if err := msgpack.Unmarshal(out, &html); err != nil {
		t.Fatalf("unmarshal admin fragment: %v", err)
	}
	if want := "<h1>tiers</h1>"; html != want {
		t.Errorf("admin_fragment = %q, want %q (msgpack fallback path)", html, want)
	}
}
