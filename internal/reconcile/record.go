package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
)

type RecordStatus string

const (
	StatusStarted    RecordStatus = "started"
	StatusCompleted  RecordStatus = "completed"
	StatusCommitted  RecordStatus = "committed"
	StatusRolledBack RecordStatus = "rolled_back"
)

type GraphDescriptor struct {
	Revision string            `json:"revision"`
	Modules  map[string]string `json:"modules"`
}
type Record struct {
	TransactionID   string          `json:"transaction_id"`
	Stage           Stage           `json:"stage"`
	Status          RecordStatus    `json:"status"`
	Previous        GraphDescriptor `json:"previous"`
	Desired         GraphDescriptor `json:"desired"`
	Changes         ChangeSet       `json:"changes"`
	SnapshotRef     string          `json:"snapshot_ref,omitempty"`
	LockDigest      string          `json:"lock_digest,omitempty"`
	ArtifactDigests []string        `json:"artifact_digests,omitempty"`
	FusionDigests   []string        `json:"fusion_digests,omitempty"`
	Error           string          `json:"error,omitempty"`
}
type Evidence struct {
	LockDigest                     string
	ArtifactDigests, FusionDigests []string
}
type Recorder interface {
	Record(context.Context, Record) error
}
type SnapshotReference interface{ DeploymentSnapshotReference() string }

var transactionSequence atomic.Uint64

func newTransactionID() string {
	return fmt.Sprintf("%d-%d", time.Now().UTC().UnixNano(), transactionSequence.Add(1))
}
func deploymentDescriptor(d Deployment) GraphDescriptor { return graphDescriptor(d.Plan, d.Revisions) }
func graphDescriptor(p *dependency.Plan, revisions map[string]string) GraphDescriptor {
	type node struct {
		ID           string   `json:"id"`
		Dependencies []string `json:"dependencies"`
		Revision     string   `json:"revision"`
	}
	nodes := make([]node, 0, len(p.StartOrder()))
	mods := make(map[string]string, len(revisions))
	for _, id := range p.StartOrder() {
		n, _ := p.Node(id)
		nodes = append(nodes, node{id, n.Dependencies(), revisions[id]})
		mods[id] = revisions[id]
	}
	b, _ := json.Marshal(nodes)
	sum := sha256.Sum256(b)
	return GraphDescriptor{Revision: hex.EncodeToString(sum[:]), Modules: mods}
}
