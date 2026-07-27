package run

import (
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

var runtimeCapabilityProviders capabilityProviderSelector

// InstallCapabilityProviders installs the deployment's exact
// capability-name-to-provider contract. It must be called before starting any
// Pulp application or legacy manifest runtime. Setup and Register callbacks see
// only the selected capabilities.
//
// A repeated identical installation is idempotent. Replacing a contract or
// installing one after runtime selection has begun is rejected.
func InstallCapabilityProviders(providers map[string]string) error {
	return runtimeCapabilityProviders.install(ext.All(), providers)
}

func selectedRuntimeCapabilities() ([]ext.Capability, error) {
	return runtimeCapabilityProviders.selectCapabilities(ext.All())
}

type capabilityProviderSelector struct {
	mu        sync.Mutex
	providers map[string]string
	installed bool
	used      bool
}

func (s *capabilityProviderSelector) install(registered []ext.Capability, providers map[string]string) error {
	if len(providers) == 0 {
		return errors.New("capability provider contract is empty")
	}
	cloned := maps.Clone(providers)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used {
		return errors.New("capability providers were already selected by a runtime")
	}
	if s.installed {
		if maps.Equal(s.providers, cloned) {
			return nil
		}
		return errors.New("a different capability provider contract is already installed")
	}
	if _, err := ext.SelectCapabilities(registered, providers); err != nil {
		return fmt.Errorf("install capability providers: %w", err)
	}
	s.providers = cloned
	s.installed = true
	return nil
}

func (s *capabilityProviderSelector) selectCapabilities(registered []ext.Capability) ([]ext.Capability, error) {
	s.mu.Lock()
	s.used = true
	providers := maps.Clone(s.providers)
	s.mu.Unlock()

	selected, err := ext.SelectCapabilities(registered, providers)
	if err != nil {
		return nil, fmt.Errorf("select runtime capability providers: %w", err)
	}
	return selected, nil
}
