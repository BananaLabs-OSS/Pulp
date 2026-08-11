package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// OperatorCommandDescriptor is a deliberately in-process, app-local operator
// entrypoint. Registering or binding it never invokes the provider; a trusted
// embedding process must explicitly call InvokeOperatorCommand.
type OperatorCommandDescriptor struct {
	Name            string
	Application     ApplicationIdentity
	Cell            string
	Provider        string
	MaxRequestBytes int
}

type operatorCommandRegistry struct {
	mu       sync.Mutex
	commands map[string]OperatorCommandDescriptor
	active   map[string]ApplicationProviderAccess
}

func newOperatorCommandRegistry() *operatorCommandRegistry {
	return &operatorCommandRegistry{commands: map[string]OperatorCommandDescriptor{}, active: map[string]ApplicationProviderAccess{}}
}

func (r *operatorCommandRegistry) register(d OperatorCommandDescriptor) error {
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Name) != d.Name || len(d.Name) > 128 ||
		strings.TrimSpace(d.Application.ApplicationID) == "" || strings.TrimSpace(d.Application.InstanceID) == "" ||
		strings.TrimSpace(d.Cell) == "" || strings.TrimSpace(d.Provider) == "" || d.MaxRequestBytes <= 0 || d.MaxRequestBytes > 1<<20 {
		return errors.New("operator command: descriptor is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.commands[d.Name]; exists {
		return fmt.Errorf("operator command %q is already registered", d.Name)
	}
	r.commands[d.Name] = d
	return nil
}

func (r *operatorCommandRegistry) bind(identity ApplicationIdentity, caller ApplicationProviderAccess) error {
	if caller == nil || caller.Identity() != identity { return errors.New("operator command: provider access identity mismatch") }
	r.mu.Lock(); defer r.mu.Unlock()
	for name, d := range r.commands { if d.Application == identity { r.active[name] = caller } }
	return nil
}

func (r *operatorCommandRegistry) unbind(identity ApplicationIdentity) {
	r.mu.Lock(); defer r.mu.Unlock()
	for name, d := range r.commands { if d.Application == identity { delete(r.active, name) } }
}

func (r *operatorCommandRegistry) invoke(ctx context.Context, name string, request []byte) ([]byte, error) {
	r.mu.Lock()
	d, exists := r.commands[name]
	caller := r.active[name]
	r.mu.Unlock()
	if !exists { return nil, fmt.Errorf("operator command %q is not registered", name) }
	if caller == nil { return nil, fmt.Errorf("operator command %q is not active", name) }
	if len(request) > d.MaxRequestBytes { return nil, fmt.Errorf("operator command %q request exceeds limit", name) }
	return caller.CallProvider(ctx, d.Cell, d.Provider, append([]byte(nil), request...))
}

var deploymentOperatorCommands = newOperatorCommandRegistry()

func RegisterOperatorCommand(d OperatorCommandDescriptor) error { return deploymentOperatorCommands.register(d) }
func InvokeOperatorCommand(ctx context.Context, name string, request []byte) ([]byte, error) { return deploymentOperatorCommands.invoke(ctx, name, request) }
