// Package fusion turns logical cell placement requests into a deterministic
// physical execution plan. It is intentionally conservative: every rejected
// request remains an isolated cell, so planning can never broaden authority
// or silently change a failure boundary.
package fusion

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// Plan is the runtime-layout decision for one application.
type Plan struct {
	Groups   []Group
	Isolated []Decision
}

// Group is an accepted fusion domain. The current runtime reports this plan
// and executes its members through the existing isolated fallback until a
// matching fused artifact is supplied by the Pulp fusible compiler backend.
type Group struct {
	Name    string
	ABI     string
	Members []*manifest.CellSpec
}

// Decision explains why a logical cell remains isolated.
type Decision struct {
	Cell   *manifest.CellSpec
	Reason string
}

// Build returns a stable, fail-closed plan. Equal capability sets are required
// because a fused instance receives one import set: allowing different sets
// would turn another cell's host authority into ambient authority.
func Build(cells []*manifest.CellSpec) Plan {
	requested := make(map[string][]*manifest.CellSpec)
	plan := Plan{}
	for _, cell := range cells {
		if cell == nil || cell.Execution.Mode != manifest.ExecutionFusible {
			if cell != nil {
				plan.Isolated = append(plan.Isolated, Decision{Cell: cell, Reason: "execution mode is isolated"})
			}
			continue
		}
		key := cell.Execution.Group + "\x00" + cell.Execution.ABI
		requested[key] = append(requested[key], cell)
	}
	keys := make([]string, 0, len(requested))
	for key := range requested {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		members := requested[key]
		sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
		if len(members) < 2 {
			plan.Isolated = append(plan.Isolated, Decision{Cell: members[0], Reason: "fusion group has fewer than two members"})
			continue
		}
		if reason := compatible(members); reason != "" {
			for _, member := range members {
				plan.Isolated = append(plan.Isolated, Decision{Cell: member, Reason: reason})
			}
			continue
		}
		plan.Groups = append(plan.Groups, Group{Name: members[0].Execution.Group, ABI: members[0].Execution.ABI, Members: members})
	}
	sort.Slice(plan.Isolated, func(i, j int) bool { return plan.Isolated[i].Cell.Name < plan.Isolated[j].Cell.Name })
	return plan
}

func compatible(members []*manifest.CellSpec) string {
	base := members[0]
	v2 := base.Execution.ABI == MemberABIV2
	if base.DedicatedThread {
		return "dedicated_thread cells are not fusible"
	}
	if base.Snapshotable && !v2 {
		return "snapshotable cells require per-member snapshot ABI support"
	}
	for _, cell := range members[1:] {
		if cell.DedicatedThread {
			return "dedicated_thread cells are not fusible"
		}
		if cell.Snapshotable && !v2 {
			return "snapshotable cells require per-member snapshot ABI support"
		}
		if cell.Restart != base.Restart {
			return "restart policy differs within fusion group"
		}
		if cell.MaxMemoryPages != base.MaxMemoryPages {
			return "memory limit differs within fusion group"
		}
		if cell.CallTimeoutMS != base.CallTimeoutMS {
			return "call timeout differs within fusion group"
		}
		if !sameSet(cell.Capabilities, base.Capabilities) {
			return "capability set differs within fusion group"
		}
		// Fusion ABI v1 passes one Init payload to every registered package.
		// Requiring identical config prevents physical placement from silently
		// changing a member's logical configuration.
		if !v2 && !reflect.DeepEqual(cell.Config, base.Config) {
			return "config differs within fusion group"
		}
	}
	return ""
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := seen[value]; !ok {
			return false
		}
	}
	return true
}

func (p Plan) String() string {
	groups := make([]string, 0, len(p.Groups))
	for _, group := range p.Groups {
		names := make([]string, len(group.Members))
		for i, member := range group.Members {
			names[i] = member.Name
		}
		groups = append(groups, fmt.Sprintf("%s[%s]", group.Name, strings.Join(names, ",")))
	}
	return strings.Join(groups, "; ")
}
