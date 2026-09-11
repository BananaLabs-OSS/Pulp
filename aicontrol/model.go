// Package aicontrol defines Pulp's protocol-neutral control plane for AI and
// other automation. Transports such as MCP are adapters to this package; they
// are not trusted deployment authorities.
package aicontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Digest string

type FileDiff struct{ Path, BeforeDigest, AfterDigest string }
type ContractDiff struct{ Name, Before, After string }
type CapabilityDiff struct{ Module, Capability, Change string }
type GraphNode struct {
	ID, Revision string
	DependsOn    []string
}

// Draft is untrusted proposed input. NewProposal validates, sorts, copies and
// seals it into a revision-bound value.
type Draft struct {
	BaseRevision string
	Intent       string
	Graph        []GraphNode
	Lua          []FileDiff
	Contracts    []ContractDiff
	Capabilities []CapabilityDiff
}

type Proposal struct {
	digest               Digest
	baseRevision, intent string
	graph                []GraphNode
	lua                  []FileDiff
	contracts            []ContractDiff
	capabilities         []CapabilityDiff
}

func NewProposal(d Draft) (Proposal, error) {
	d.BaseRevision, d.Intent = strings.TrimSpace(d.BaseRevision), strings.TrimSpace(d.Intent)
	if d.BaseRevision == "" || d.Intent == "" {
		return Proposal{}, errors.New("proposal requires base revision and intent")
	}
	seen := map[string]bool{}
	for i := range d.Graph {
		n := &d.Graph[i]
		n.ID, n.Revision = strings.TrimSpace(n.ID), strings.TrimSpace(n.Revision)
		if n.ID == "" || n.Revision == "" || seen[n.ID] {
			return Proposal{}, fmt.Errorf("invalid or duplicate graph node %q", n.ID)
		}
		seen[n.ID] = true
		n.DependsOn = append([]string(nil), n.DependsOn...)
		slices.Sort(n.DependsOn)
		for j := 1; j < len(n.DependsOn); j++ {
			if n.DependsOn[j] == n.DependsOn[j-1] {
				return Proposal{}, fmt.Errorf("duplicate dependency %q", n.DependsOn[j])
			}
		}
	}
	for _, n := range d.Graph {
		for _, dep := range n.DependsOn {
			if !seen[dep] {
				return Proposal{}, fmt.Errorf("node %q depends on unknown node %q", n.ID, dep)
			}
		}
	}
	slices.SortFunc(d.Graph, func(a, b GraphNode) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(d.Lua, func(a, b FileDiff) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(d.Contracts, func(a, b ContractDiff) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(d.Capabilities, func(a, b CapabilityDiff) int {
		if c := strings.Compare(a.Module, b.Module); c != 0 {
			return c
		}
		return strings.Compare(a.Capability, b.Capability)
	})
	p := Proposal{baseRevision: d.BaseRevision, intent: d.Intent, graph: cloneGraph(d.Graph), lua: append([]FileDiff(nil), d.Lua...), contracts: append([]ContractDiff(nil), d.Contracts...), capabilities: append([]CapabilityDiff(nil), d.Capabilities...)}
	p.digest = digest(p.canonical())
	return p, nil
}

func (p Proposal) Digest() Digest                { return p.digest }
func (p Proposal) BaseRevision() string          { return p.baseRevision }
func (p Proposal) Intent() string                { return p.intent }
func (p Proposal) Graph() []GraphNode            { return cloneGraph(p.graph) }
func (p Proposal) LuaDiffs() []FileDiff          { return append([]FileDiff(nil), p.lua...) }
func (p Proposal) ContractDiffs() []ContractDiff { return append([]ContractDiff(nil), p.contracts...) }
func (p Proposal) CapabilityDiffs() []CapabilityDiff {
	return append([]CapabilityDiff(nil), p.capabilities...)
}
func (p Proposal) canonical() any {
	return struct {
		Schema       int `json:"schema"`
		Base, Intent string
		Graph        []GraphNode
		Lua          []FileDiff
		Contracts    []ContractDiff
		Capabilities []CapabilityDiff
	}{1, p.baseRevision, p.intent, p.graph, p.lua, p.contracts, p.capabilities}
}

type EvidenceItem struct {
	Kind, Name, Digest string
	Passed             bool
}
type Evidence struct {
	Proposal       Digest
	Items          []EvidenceItem
	ArtifactDigest string
	digest         Digest
}

func NewEvidence(proposal Proposal, items []EvidenceItem, artifactDigest string) (Evidence, error) {
	if proposal.digest == "" {
		return Evidence{}, errors.New("evidence requires sealed proposal")
	}
	items = append([]EvidenceItem(nil), items...)
	slices.SortFunc(items, func(a, b EvidenceItem) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, i := range items {
		if i.Kind == "" || i.Name == "" || i.Digest == "" {
			return Evidence{}, errors.New("evidence item requires kind, name, and digest")
		}
	}
	e := Evidence{Proposal: proposal.digest, Items: items, ArtifactDigest: artifactDigest}
	e.digest = digest(struct {
		Schema   int
		Proposal Digest
		Items    []EvidenceItem
		Artifact string
	}{1, e.Proposal, e.Items, e.ArtifactDigest})
	return e, nil
}
func (e Evidence) Digest() Digest            { return e.digest }
func (e Evidence) CopyItems() []EvidenceItem { return append([]EvidenceItem(nil), e.Items...) }
func (e Evidence) computedDigest() Digest {
	return digest(struct {
		Schema   int
		Proposal Digest
		Items    []EvidenceItem
		Artifact string
	}{1, e.Proposal, e.Items, e.ArtifactDigest})
}

type Approval struct {
	Proposal, Evidence, Policy Digest
	Approver, Nonce            string
	ExpiresAt                  time.Time
	Signature                  []byte
}
type ApprovalVerifier interface{ VerifyApproval(Approval) error }

func digest(v any) Digest {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return Digest(hex.EncodeToString(h[:]))
}
func cloneGraph(in []GraphNode) []GraphNode {
	out := make([]GraphNode, len(in))
	for i, n := range in {
		out[i] = n
		out[i].DependsOn = append([]string(nil), n.DependsOn...)
	}
	return out
}
