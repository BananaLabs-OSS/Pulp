// A pilot Evolution gene cell that routes exactly ONE sibling method —
// gene.catalog — over the typed canonical-ABI path (gene.ProvideCatalogTyped +
// the pulp_post_return export below), while EVERY other gene method stays on
// the unchanged msgpack Provider path wired by gene.Register. Used by the Pulp
// host harness (cellharness_genetyped_test.go) to prove:
//
//   - gene.catalog round-trips a rich RegistrationInfo typed end-to-end via
//     Cell.CallTyped, with the response tree freed to zero leaked pins; and
//   - gene.admin_fragment (and the rest) still round-trips via the opaque
//     Cell.Call msgpack path, untouched by the graft.
package main

import (
	"github.com/BananaLabs-OSS/Fiber/pulp"
	"github.com/BananaLabs-OSS/Fiber/pulp/gene"
)

func main() {}

// pilotGene is a minimal Gene: Catalog returns a representative catalog and
// AdminFragment echoes a tab; the remaining hooks are no-ops (enough to satisfy
// the interface so gene.Register can wire the full msgpack surface).
type pilotGene struct{}

func (pilotGene) Catalog() gene.RegistrationInfo {
	return gene.RegistrationInfo{
		Name:    "pilot",
		Version: "1.2.3",
		SKUs: []gene.SKU{
			{
				ID:          "sku-basic",
				Name:        "Basic Session",
				Description: "one server",
				PriceCents:  1400,
				Currency:    "usd",
				Metadata:    map[string]string{"tier": "basic", "region": "us"},
			},
			{
				ID:         "sku-plus",
				Name:       "Plus Session",
				PriceCents: 2900,
				Currency:   "usd",
				// nil Metadata: exercises the empty-map (null list) arm.
			},
		},
		Routes: []gene.RouteDecl{
			{Method: "GET", Path: "/api/voucher/:id/redeem"},
			{Method: "POST", Path: "/api/session/:id/deploy"},
		},
		AdminTabs: []gene.AdminTab{
			{Key: "tiers", Label: "Tier Management", Icon: "layers", Order: 1},
		},
		EmailTemplates: []string{"purchased", "ready", "expired"},
	}
}

func (pilotGene) ValidatePurchase(gene.PurchaseRequest) (gene.ValidatedOrder, error) {
	return gene.ValidatedOrder{}, nil
}
func (pilotGene) OnOrderPaid(gene.OrderView) error                { return nil }
func (pilotGene) FulfillmentSpec(string) (gene.ServerSpec, error) { return gene.ServerSpec{}, nil }
func (pilotGene) OnServerReady(string, string) error              { return nil }
func (pilotGene) OnOrderRefunded(gene.OrderView) error            { return nil }
func (pilotGene) OnOrderExpired(gene.OrderView) error             { return nil }
func (pilotGene) HandleRoute(gene.HTTPRequest) (gene.HTTPResponse, error) {
	return gene.HTTPResponse{}, nil
}
func (pilotGene) AdminFragment(tab string) (string, error)   { return "<h1>" + tab + "</h1>", nil }
func (pilotGene) AdminAction(string, []byte) ([]byte, error) { return nil, nil }
func (pilotGene) EmailTemplate(string, gene.OrderView) (gene.EmailTemplate, error) {
	return gene.EmailTemplate{}, nil
}

func init() {
	g := pilotGene{}
	gene.Register(g)            // full msgpack surface (untouched fallback path)
	gene.ProvideCatalogTyped(g) // ONE method over the typed canonical-ABI path
}

// pulp_post_return tree-frees the gene.catalog response after the host lifts
// it. It lives HERE in package main (not in the shared gene package) so that
// importing gene never adds this export to a msgpack-only gene — preserving
// Pulp's post_return gate.
//
//go:wasmexport pulp_post_return
func pulpPostReturn(recPtr, recLen uint32) {
	_ = recLen
	gene.CatalogPostReturn(recPtr)
}

// probe_alloc_live exposes Fiber's alloc-table pin count so the host test can
// assert the response tree returns to baseline after pulp_post_return.
//
//go:wasmexport probe_alloc_live
func probeAllocLive() uint32 { return pulp.AllocLive() }
