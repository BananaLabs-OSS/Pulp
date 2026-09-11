package aicontrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

type PolicyDecision struct {
	PolicyDigest     Digest
	Allowed          bool
	Reasons          []string
	RequiresApproval bool
}
type Policy interface {
	Evaluate(context.Context, Proposal, Evidence) (PolicyDecision, error)
}
type Builder interface {
	Build(context.Context, Proposal) (artifactDigest string, err error)
}
type Tester interface {
	Test(context.Context, Proposal, string) ([]EvidenceItem, error)
}
type Deployer interface {
	CurrentRevision(context.Context) (string, error)
	Deploy(context.Context, Proposal, Evidence) (deploymentID string, err error)
	Rollback(context.Context, string) error
}

type Event struct {
	At                    time.Time
	Operation             string
	Proposal              Digest
	Actor, Result, Detail string
}
type AuditSink interface {
	Record(context.Context, Event) error
}
type Clock func() time.Time

// Controller is safe for concurrent callers and serializes authority-changing
// operations. It deliberately has no transport assumptions, leaving a narrow
// seam for MCP, HTTP, CLI, or an in-process AI driver.
type Controller struct {
	builder  Builder
	tester   Tester
	deployer Deployer
	policy   Policy
	verifier ApprovalVerifier
	audit    AuditSink
	clock    Clock
	mutate   sync.Mutex
}

func NewController(b Builder, t Tester, d Deployer, p Policy, v ApprovalVerifier, a AuditSink) (*Controller, error) {
	if b == nil || t == nil || d == nil || p == nil || v == nil || a == nil {
		return nil, errors.New("AI controller requires builder, tester, deployer, policy, approval verifier, and audit sink")
	}
	return &Controller{builder: b, tester: t, deployer: d, policy: p, verifier: v, audit: a, clock: time.Now}, nil
}

func (c *Controller) Assess(ctx context.Context, p Proposal, actor string) (Evidence, PolicyDecision, error) {
	if p.digest == "" {
		return Evidence{}, PolicyDecision{}, errors.New("assess requires sealed proposal")
	}
	artifact, err := c.builder.Build(ctx, p)
	if err != nil {
		c.record(ctx, "assess", p.digest, actor, "failed", err.Error())
		return Evidence{}, PolicyDecision{}, fmt.Errorf("build candidate: %w", err)
	}
	items, err := c.tester.Test(ctx, p, artifact)
	if err != nil {
		c.record(ctx, "assess", p.digest, actor, "failed", err.Error())
		return Evidence{}, PolicyDecision{}, fmt.Errorf("test candidate: %w", err)
	}
	e, err := NewEvidence(p, items, artifact)
	if err != nil {
		return Evidence{}, PolicyDecision{}, err
	}
	decision, err := c.policy.Evaluate(ctx, p, e)
	if err != nil {
		return Evidence{}, PolicyDecision{}, fmt.Errorf("evaluate policy: %w", err)
	}
	if decision.PolicyDigest == "" {
		return Evidence{}, PolicyDecision{}, errors.New("policy returned empty digest")
	}
	c.record(ctx, "assess", p.digest, actor, "complete", string(e.digest))
	return e, decision, nil
}

func (c *Controller) Deploy(ctx context.Context, p Proposal, e Evidence, d PolicyDecision, a *Approval, actor string) (string, error) {
	c.mutate.Lock()
	defer c.mutate.Unlock()
	fail := func(err error) (string, error) {
		c.record(ctx, "deploy", p.digest, actor, "denied", err.Error())
		return "", err
	}
	if p.digest == "" || e.Proposal != p.digest || e.digest == "" {
		return fail(errors.New("evidence is not bound to proposal"))
	}
	if e.computedDigest() != e.digest {
		return fail(errors.New("evidence content was modified after assessment"))
	}
	verifiedDecision, err := c.policy.Evaluate(ctx, p, e)
	if err != nil {
		return fail(fmt.Errorf("re-evaluate policy: %w", err))
	}
	if verifiedDecision.PolicyDigest != d.PolicyDigest || verifiedDecision.Allowed != d.Allowed || verifiedDecision.RequiresApproval != d.RequiresApproval || !slices.Equal(verifiedDecision.Reasons, d.Reasons) {
		return fail(errors.New("policy decision does not match current policy evaluation"))
	}
	if !d.Allowed {
		return fail(fmt.Errorf("policy denied proposal: %v", d.Reasons))
	}
	for _, i := range e.Items {
		if !i.Passed {
			return fail(fmt.Errorf("evidence %s/%s failed", i.Kind, i.Name))
		}
	}
	if d.RequiresApproval {
		if a == nil {
			return fail(errors.New("policy requires approval"))
		}
		if a.Proposal != p.digest || a.Evidence != e.digest || a.Policy != d.PolicyDigest {
			return fail(errors.New("approval binding mismatch"))
		}
		if a.Approver == "" || a.Nonce == "" || !a.ExpiresAt.After(c.clock()) {
			return fail(errors.New("approval is incomplete or expired"))
		}
		if err := c.verifier.VerifyApproval(*a); err != nil {
			return fail(fmt.Errorf("verify approval: %w", err))
		}
	}
	current, err := c.deployer.CurrentRevision(ctx)
	if err != nil {
		return fail(fmt.Errorf("read current revision: %w", err))
	}
	if current != p.baseRevision {
		return fail(fmt.Errorf("stale proposal: based on %q, current is %q", p.baseRevision, current))
	}
	id, err := c.deployer.Deploy(ctx, p, e)
	if err != nil {
		return fail(fmt.Errorf("deploy candidate: %w", err))
	}
	c.record(ctx, "deploy", p.digest, actor, "complete", id)
	return id, nil
}
func (c *Controller) Rollback(ctx context.Context, deploymentID, actor string) error {
	c.mutate.Lock()
	defer c.mutate.Unlock()
	if deploymentID == "" {
		return errors.New("rollback requires deployment ID")
	}
	err := c.deployer.Rollback(ctx, deploymentID)
	result := "complete"
	detail := deploymentID
	if err != nil {
		result = "failed"
		detail = err.Error()
	}
	c.record(ctx, "rollback", "", actor, result, detail)
	return err
}
func (c *Controller) record(ctx context.Context, op string, p Digest, actor, result, detail string) {
	_ = c.audit.Record(context.WithoutCancel(ctx), Event{At: c.clock().UTC(), Operation: op, Proposal: p, Actor: actor, Result: result, Detail: detail})
}

// StaticPolicy is a useful baseline policy. More sophisticated engines can
// implement Policy without changing callers or transports.
type StaticPolicy struct {
	Digest              Digest
	AllowedCapabilities []string
	RequireApproval     bool
}

func (p StaticPolicy) Evaluate(_ context.Context, x Proposal, _ Evidence) (PolicyDecision, error) {
	allowed := append([]string(nil), p.AllowedCapabilities...)
	slices.Sort(allowed)
	var reasons []string
	for _, c := range x.capabilities {
		if c.Change == "add" && !slices.Contains(allowed, c.Capability) {
			reasons = append(reasons, "capability not allowed: "+c.Module+"/"+c.Capability)
		}
	}
	return PolicyDecision{PolicyDigest: p.Digest, Allowed: len(reasons) == 0, Reasons: reasons, RequiresApproval: p.RequireApproval}, nil
}
