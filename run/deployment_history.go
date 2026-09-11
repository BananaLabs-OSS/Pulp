package run

import "github.com/BananaLabs-OSS/Pulp/internal/reconcile"

type DeploymentHistory struct{ journal *reconcile.FileJournal }

func OpenDeploymentHistory(path string) (*DeploymentHistory, error) {
	j, e := reconcile.OpenFileJournal(path)
	if e != nil {
		return nil, e
	}
	return &DeploymentHistory{journal: j}, nil
}
func (h *DeploymentHistory) Close() error { return h.journal.Close() }

type DeploymentRecovery struct {
	IncompleteTransactions     []string
	LastCommittedGraphRevision string
	RollbackGraphRevision      string
}

func (h *DeploymentHistory) Recovery() DeploymentRecovery {
	r := h.journal.RecoveryState()
	o := DeploymentRecovery{}
	for _, e := range r.Incomplete {
		o.IncompleteTransactions = append(o.IncompleteTransactions, e.Record.TransactionID)
	}
	if r.LastCommitted != nil {
		o.LastCommittedGraphRevision = r.LastCommitted.Record.Desired.Revision
		o.RollbackGraphRevision = r.LastCommitted.Record.Previous.Revision
	}
	return o
}

// NewDurableLiveRuntimeController records every live transition. On restart,
// callers must resolve any Recovery().IncompleteTransactions before admission.
func NewDurableLiveRuntimeController(current []LiveModule, value any, lifecycle LiveRuntimeLifecycle, history *DeploymentHistory) (*LiveRuntimeController, error) {
	if history == nil {
		return newLiveRuntimeController(current, value, lifecycle, nil)
	}
	return newLiveRuntimeController(current, value, lifecycle, history.journal)
}
