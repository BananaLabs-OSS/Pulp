package ext

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// PlacementGrant is host-owned authority assigned to one exact application
// cell placement. It is never populated from a guest cell manifest.
type PlacementGrant struct {
	Scope      Scope
	Capability string
	Resource   string
	Rights     []string
	Attributes map[string]string
}

// PlacementGrantResolver exposes grants without allowing extensions or guests
// to mutate the host's grant registry.
type PlacementGrantResolver interface {
	ResolvePlacementGrant(scope Scope, capability string) (PlacementGrant, bool)
}

type placementGrantKey struct {
	scope      Scope
	capability string
}
type StaticPlacementGrants struct {
	grants map[placementGrantKey]PlacementGrant
}

func NewStaticPlacementGrants(input []PlacementGrant) (*StaticPlacementGrants, error) {
	out := &StaticPlacementGrants{grants: make(map[placementGrantKey]PlacementGrant, len(input))}
	for _, raw := range input {
		grant, err := normalizePlacementGrant(raw)
		if err != nil {
			return nil, err
		}
		key := placementGrantKey{grant.Scope, grant.Capability}
		if _, exists := out.grants[key]; exists {
			return nil, fmt.Errorf("duplicate placement grant for %s capability %q", grant.Scope.RoutingID(), grant.Capability)
		}
		out.grants[key] = grant
	}
	return out, nil
}

func (s *StaticPlacementGrants) ResolvePlacementGrant(scope Scope, capability string) (PlacementGrant, bool) {
	if s == nil || scope.Validate() != nil {
		return PlacementGrant{}, false
	}
	grant, ok := s.grants[placementGrantKey{scope, strings.TrimSpace(strings.ToLower(capability))}]
	if !ok {
		return PlacementGrant{}, false
	}
	return clonePlacementGrant(grant), true
}

func normalizePlacementGrant(g PlacementGrant) (PlacementGrant, error) {
	if err := g.Scope.Validate(); err != nil {
		return PlacementGrant{}, fmt.Errorf("placement grant scope: %w", err)
	}
	g.Capability = strings.TrimSpace(strings.ToLower(g.Capability))
	g.Resource = strings.TrimSpace(g.Resource)
	if g.Capability == "" || g.Resource == "" {
		return PlacementGrant{}, errors.New("placement grant requires capability and resource")
	}
	seen := map[string]bool{}
	rights := make([]string, 0, len(g.Rights))
	for _, raw := range g.Rights {
		right := strings.TrimSpace(strings.ToLower(raw))
		if right == "" || strings.ContainsAny(right, " \t\r\n") {
			return PlacementGrant{}, errors.New("placement grant contains invalid right")
		}
		if seen[right] {
			return PlacementGrant{}, fmt.Errorf("duplicate placement right %q", right)
		}
		seen[right] = true
		rights = append(rights, right)
	}
	if len(rights) == 0 {
		return PlacementGrant{}, errors.New("placement grant requires at least one right")
	}
	sort.Strings(rights)
	g.Rights = rights
	attrs := make(map[string]string, len(g.Attributes))
	for k, v := range g.Attributes {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" || strings.ContainsAny(k, " \t\r\n") {
			return PlacementGrant{}, errors.New("placement grant contains invalid attribute")
		}
		if _, ok := attrs[k]; ok {
			return PlacementGrant{}, fmt.Errorf("duplicate placement attribute %q", k)
		}
		attrs[k] = v
	}
	g.Attributes = attrs
	return g, nil
}
func clonePlacementGrant(g PlacementGrant) PlacementGrant {
	g.Rights = append([]string(nil), g.Rights...)
	original := g.Attributes
	g.Attributes = map[string]string{}
	for k, v := range original {
		g.Attributes[k] = v
	}
	return g
}
func (g PlacementGrant) Allows(right string) bool {
	right = strings.TrimSpace(strings.ToLower(right))
	i := sort.SearchStrings(g.Rights, right)
	return i < len(g.Rights) && g.Rights[i] == right
}
