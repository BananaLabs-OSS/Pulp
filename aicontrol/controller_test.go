package aicontrol

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeOps struct {
	revision           string
	deploys, rollbacks int
}

func (f *fakeOps) Build(context.Context, Proposal) (string, error) {
	return strings.Repeat("a", 64), nil
}
func (f *fakeOps) Test(context.Context, Proposal, string) ([]EvidenceItem, error) {
	return []EvidenceItem{{Kind: "test", Name: "unit", Digest: strings.Repeat("b", 64), Passed: true}}, nil
}
func (f *fakeOps) CurrentRevision(context.Context) (string, error) { return f.revision, nil }
func (f *fakeOps) Deploy(context.Context, Proposal, Evidence) (string, error) {
	f.deploys++
	return "deployment-1", nil
}
func (f *fakeOps) Rollback(context.Context, string) error { f.rollbacks++; return nil }

type verifier struct {
	err   error
	calls int
}

func (v *verifier) VerifyApproval(Approval) error { v.calls++; return v.err }

type audit struct{ events []Event }

func (a *audit) Record(_ context.Context, e Event) error { a.events = append(a.events, e); return nil }

func proposal(t *testing.T) Proposal {
	t.Helper()
	p, e := NewProposal(Draft{BaseRevision: "rev-1", Intent: "upgrade rules", Graph: []GraphNode{{ID: "game", Revision: "2", DependsOn: []string{"engine"}}, {ID: "engine", Revision: "1"}}, Lua: []FileDiff{{Path: "compose.lua", BeforeDigest: "1", AfterDigest: "2"}}, Capabilities: []CapabilityDiff{{Module: "game", Capability: "network", Change: "add"}}})
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestProposalIsCanonicalAndImmutable(t *testing.T) {
	d := Draft{BaseRevision: "r", Intent: "x", Graph: []GraphNode{{ID: "z", Revision: "1", DependsOn: []string{"a"}}, {ID: "a", Revision: "1"}}}
	p1, e := NewProposal(d)
	if e != nil {
		t.Fatal(e)
	}
	d.Graph[0].ID = "mutated"
	got := p1.Graph()
	got[0].ID = "also-mutated"
	p2, e := NewProposal(Draft{BaseRevision: "r", Intent: "x", Graph: []GraphNode{{ID: "a", Revision: "1"}, {ID: "z", Revision: "1", DependsOn: []string{"a"}}}})
	if e != nil {
		t.Fatal(e)
	}
	if p1.Digest() != p2.Digest() {
		t.Fatalf("canonical digests differ: %s %s", p1.Digest(), p2.Digest())
	}
	if p1.Graph()[0].ID != "a" {
		t.Fatal("proposal storage was mutable")
	}
}

func TestControllerBindsApprovalAndDeploys(t *testing.T) {
	f := &fakeOps{revision: "rev-1"}
	v := &verifier{}
	a := &audit{}
	policy := StaticPolicy{Digest: "policy-1", AllowedCapabilities: []string{"network"}, RequireApproval: true}
	c, e := NewController(f, f, f, policy, v, a)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c.clock = func() time.Time { return now }
	p := proposal(t)
	proof, decision, e := c.Assess(context.Background(), p, "agent")
	if e != nil {
		t.Fatal(e)
	}
	approval := Approval{Proposal: p.Digest(), Evidence: proof.Digest(), Policy: decision.PolicyDigest, Approver: "human", Nonce: "unique", ExpiresAt: now.Add(time.Hour), Signature: []byte("signature")}
	id, e := c.Deploy(context.Background(), p, proof, decision, &approval, "agent")
	if e != nil {
		t.Fatal(e)
	}
	if id != "deployment-1" || f.deploys != 1 || v.calls != 1 {
		t.Fatalf("deployment=%q deploys=%d verifies=%d", id, f.deploys, v.calls)
	}
	if len(a.events) != 2 || a.events[0].Operation != "assess" || a.events[1].Result != "complete" {
		t.Fatalf("audit: %#v", a.events)
	}
}

func TestControllerRejectsStaleTamperedAndFailedEvidence(t *testing.T) {
	base := proposal(t)
	for _, tc := range []struct {
		name   string
		mutate func(*fakeOps, *Evidence, *PolicyDecision, *Approval)
		want   string
	}{
		{"stale", func(f *fakeOps, _ *Evidence, _ *PolicyDecision, _ *Approval) { f.revision = "rev-2" }, "stale proposal"},
		{"wrong evidence", func(_ *fakeOps, e *Evidence, _ *PolicyDecision, _ *Approval) { e.Proposal = "other" }, "not bound"},
		{"tampered evidence", func(_ *fakeOps, e *Evidence, _ *PolicyDecision, _ *Approval) { e.Items[0].Digest = "changed" }, "modified after assessment"},
		{"expired", func(_ *fakeOps, _ *Evidence, _ *PolicyDecision, a *Approval) { a.ExpiresAt = time.Unix(1, 0) }, "expired"},
		{"bad signature", func(_ *fakeOps, _ *Evidence, _ *PolicyDecision, _ *Approval) {}, "verify approval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeOps{revision: "rev-1"}
			v := &verifier{}
			if tc.name == "bad signature" {
				v.err = errors.New("bad")
			}
			c, _ := NewController(f, f, f, StaticPolicy{Digest: "p", AllowedCapabilities: []string{"network"}, RequireApproval: true}, v, &audit{})
			c.clock = func() time.Time { return time.Unix(100, 0) }
			e, d, err := c.Assess(context.Background(), base, "ai")
			if err != nil {
				t.Fatal(err)
			}
			ap := Approval{Proposal: base.Digest(), Evidence: e.Digest(), Policy: d.PolicyDigest, Approver: "h", Nonce: "n", ExpiresAt: time.Unix(200, 0)}
			tc.mutate(f, &e, &d, &ap)
			_, err = c.Deploy(context.Background(), base, e, d, &ap, "ai")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
			if f.deploys != 0 {
				t.Fatal("unauthorized deployment occurred")
			}
		})
	}
}

func TestStaticPolicyDeniesCapabilityEscalation(t *testing.T) {
	p := proposal(t)
	e, _ := NewEvidence(p, []EvidenceItem{{Kind: "test", Name: "x", Digest: "d", Passed: true}}, "a")
	d, err := (StaticPolicy{Digest: "v1"}).Evaluate(context.Background(), p, e)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || len(d.Reasons) != 1 {
		t.Fatalf("decision: %#v", d)
	}
}

func TestRollbackIsAudited(t *testing.T) {
	f := &fakeOps{}
	a := &audit{}
	c, _ := NewController(f, f, f, StaticPolicy{Digest: "p"}, &verifier{}, a)
	if err := c.Rollback(context.Background(), "dep-1", "operator"); err != nil {
		t.Fatal(err)
	}
	if f.rollbacks != 1 || len(a.events) != 1 || a.events[0].Operation != "rollback" {
		t.Fatalf("rollback audit: %#v", a.events)
	}
}
