package run

import (
	"fmt"
	"strings"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

// EndpointRegistry is the host-owned discovery index for scoped extension
// endpoints. It implements ext.EndpointReporter and makes an endpoint visible
// only after its listener reports Ready. A public endpoint is unique per
// application instance, so HostGateway can never accidentally choose a
// sibling cell or application as an upstream.
type EndpointRegistry struct {
	mu            sync.RWMutex
	byResource    map[ext.ResourceKey]ext.Endpoint
	byApplication map[endpointApplicationKey]ext.ResourceKey
}

type endpointApplicationKey struct {
	applicationID string
	instanceID    string
	capability    string
	name          string
}

var _ ext.EndpointReporter = (*EndpointRegistry)(nil)

// NewEndpointRegistry returns an empty, concurrency-safe endpoint index.
func NewEndpointRegistry() *EndpointRegistry {
	return &EndpointRegistry{
		byResource:    make(map[ext.ResourceKey]ext.Endpoint),
		byApplication: make(map[endpointApplicationKey]ext.ResourceKey),
	}
}

// Ready records endpoint after its listener has bound. Repeated reports for
// exactly the same scoped endpoint/address are idempotent. A second endpoint
// with the same application-instance/capability/name is rejected, even if it
// comes from another cell, because an application front door must be singular.
func (r *EndpointRegistry) Ready(endpoint ext.Endpoint) error {
	resource, application, err := endpointKeys(endpoint)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("endpoint registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byResource[resource]; ok {
		if existing.Address == endpoint.Address {
			return nil
		}
		return fmt.Errorf("endpoint %s/%s already ready at %q", endpoint.Capability, endpoint.Name, existing.Address)
	}
	if existingResource, ok := r.byApplication[application]; ok {
		existing := r.byResource[existingResource]
		return fmt.Errorf("application endpoint %s/%s already owned by cell %q at %q", endpoint.Capability, endpoint.Name, existing.Scope.CellID(), existing.Address)
	}
	r.byResource[resource] = endpoint
	r.byApplication[application] = resource
	return nil
}

// Gone removes endpoint only when it still owns the exact address reported at
// Ready. A late teardown notification therefore cannot erase a replacement.
func (r *EndpointRegistry) Gone(endpoint ext.Endpoint) {
	if r == nil {
		return
	}
	resource, application, err := endpointKeys(endpoint)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byResource[resource]; ok && existing.Address == endpoint.Address {
		delete(r.byResource, resource)
		if r.byApplication[application] == resource {
			delete(r.byApplication, application)
		}
	}
}

// ApplicationAddress returns a ready endpoint belonging to exactly one
// application instance. It is used by ApplicationHTTPRuntime after Start.
func (r *EndpointRegistry) ApplicationAddress(applicationID, instanceID, capability, name string) (string, bool) {
	if r == nil {
		return "", false
	}
	key := endpointApplicationKey{
		applicationID: strings.TrimSpace(applicationID),
		instanceID:    strings.TrimSpace(instanceID),
		capability:    strings.TrimSpace(capability),
		name:          strings.TrimSpace(name),
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	resource, ok := r.byApplication[key]
	if !ok {
		return "", false
	}
	endpoint, ok := r.byResource[resource]
	return endpoint.Address, ok
}

// RemoveApplication clears every endpoint owned by an application instance.
// Runtime shutdown calls it as a final safety net after cells have stopped.
func (r *EndpointRegistry) RemoveApplication(applicationID, instanceID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for application, resource := range r.byApplication {
		if application.applicationID != applicationID || application.instanceID != instanceID {
			continue
		}
		delete(r.byApplication, application)
		delete(r.byResource, resource)
	}
}

func endpointKeys(endpoint ext.Endpoint) (ext.ResourceKey, endpointApplicationKey, error) {
	if err := endpoint.Scope.Validate(); err != nil {
		return ext.ResourceKey{}, endpointApplicationKey{}, fmt.Errorf("endpoint scope: %w", err)
	}
	if strings.TrimSpace(endpoint.Address) == "" {
		return ext.ResourceKey{}, endpointApplicationKey{}, fmt.Errorf("endpoint address is required")
	}
	resource, err := endpoint.Scope.ResourceKey(endpoint.Capability, endpoint.Name)
	if err != nil {
		return ext.ResourceKey{}, endpointApplicationKey{}, err
	}
	return resource, endpointApplicationKey{
		applicationID: endpoint.Scope.ApplicationID(),
		instanceID:    endpoint.Scope.ApplicationInstanceID(),
		capability:    endpoint.Capability,
		name:          endpoint.Name,
	}, nil
}
