// Package composition defines language-neutral contracts used by Lua to wire
// sealed Pulp modules without giving those modules direct references to one
// another.
package composition

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Kind string

const (
	Command   Kind = "command"
	Event     Kind = "event"
	Query     Kind = "query"
	State     Kind = "state"
	Lifecycle Kind = "lifecycle"
)

var contractID = regexp.MustCompile(`^[a-z][a-z0-9-]*(?:\.[a-z][a-z0-9-]*)*\.v[1-9][0-9]*$`)

// Contract is a semantic interface. Owner is the sole state/command/query
// provider; events may be observed by multiple consumers but still have one
// authoritative emitter.
type Contract struct {
	ID      string
	Kind    Kind
	Owner   string
	Request string
	Reply   string
}

type Module struct {
	Name     string
	Provides []string
	Consumes []string
}

type Catalog struct {
	contracts map[string]Contract
	providers map[string]string
}

func Build(contracts []Contract, modules []Module) (*Catalog, error) {
	catalog := &Catalog{contracts: make(map[string]Contract, len(contracts)), providers: map[string]string{}}
	moduleNames := map[string]bool{}
	for _, module := range modules {
		if strings.TrimSpace(module.Name) == "" || moduleNames[module.Name] {
			return nil, fmt.Errorf("composition module names must be non-empty and unique: %q", module.Name)
		}
		moduleNames[module.Name] = true
	}
	for _, contract := range contracts {
		contract.ID = strings.TrimSpace(contract.ID)
		contract.Owner = strings.TrimSpace(contract.Owner)
		if !contractID.MatchString(contract.ID) {
			return nil, fmt.Errorf("contract %q must end in a non-zero .vN semantic version", contract.ID)
		}
		switch contract.Kind {
		case Command, Event, Query, State, Lifecycle:
		default:
			return nil, fmt.Errorf("contract %s has invalid kind %q", contract.ID, contract.Kind)
		}
		if _, duplicate := catalog.contracts[contract.ID]; duplicate {
			return nil, fmt.Errorf("duplicate composition contract %q", contract.ID)
		}
		if !moduleNames[contract.Owner] {
			return nil, fmt.Errorf("contract %s has unknown owner %q", contract.ID, contract.Owner)
		}
		catalog.contracts[contract.ID] = contract
		catalog.providers[contract.ID] = contract.Owner
	}
	for _, module := range modules {
		for _, provided := range module.Provides {
			contract, ok := catalog.contracts[provided]
			if !ok {
				return nil, fmt.Errorf("module %s provides undeclared contract %s", module.Name, provided)
			}
			if contract.Owner != module.Name {
				return nil, fmt.Errorf("module %s provides contract %s owned by %s", module.Name, provided, contract.Owner)
			}
		}
		for _, consumed := range module.Consumes {
			if _, ok := catalog.contracts[consumed]; !ok {
				return nil, fmt.Errorf("module %s consumes undeclared contract %s", module.Name, consumed)
			}
			if catalog.providers[consumed] == module.Name {
				return nil, fmt.Errorf("module %s cannot consume its own contract %s through composition", module.Name, consumed)
			}
		}
	}
	return catalog, nil
}

func (c *Catalog) Resolve(consumer, contractID string) (Contract, string, error) {
	contract, ok := c.contracts[contractID]
	if !ok {
		return Contract{}, "", fmt.Errorf("composition contract %q is not declared", contractID)
	}
	if consumer == contract.Owner {
		return Contract{}, "", fmt.Errorf("owner %q cannot route %s through Lua back to itself", consumer, contractID)
	}
	return contract, contract.Owner, nil
}

func (c *Catalog) IDs() []string {
	ids := make([]string, 0, len(c.contracts))
	for id := range c.contracts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
