package manifest

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// CurrentHostSchemaVersion is the pulp.host.toml schema supported by this
// Pulp host.
const CurrentHostSchemaVersion = 1

// Host is a loaded multi-application Pulp host composition. An application
// points at an ordinary pulp.app.toml; package artefacts may therefore be
// shared while every application still receives separately named instances
// and isolated storage/event scopes.
type Host struct {
	SchemaVersion int
	Name          string
	ManifestPath  string

	Applications []*HostedApplication
	// ApplicationOrder is a dependency-safe startup order. It contains the
	// same entries as Applications, with every DependsOn application before
	// its dependent.
	ApplicationOrder []*HostedApplication
	Routes           []*RouteBinding
}

// HostedApplication is one application composition assigned to a host.
type HostedApplication struct {
	ID               string
	ManifestPath     string
	Application      *Application
	Instances        []ApplicationInstance
	StorageNamespace string
	EventNamespace   string
	DependsOn        []string
}

// ApplicationInstance is one independent runtime instantiation of an
// application. Alias is the stable name used by route bindings and host
// extensions; it is never inferred from a package/cell name.
type ApplicationInstance struct {
	Ordinal int
	Alias   string
}

// RouteBinding sends a host route to one application instance.
type RouteBinding struct {
	Path        string
	Application string
	Instance    string
}

type rawHost struct {
	SchemaVersion int               `toml:"schema_version"`
	Name          string            `toml:"name"`
	Applications  []rawHostedApp    `toml:"applications"`
	Routes        []rawRouteBinding `toml:"routes"`
}

type rawHostedApp struct {
	ID               string   `toml:"id"`
	Manifest         string   `toml:"manifest"`
	Instances        int      `toml:"instances"`
	Aliases          []string `toml:"aliases"`
	StorageNamespace string   `toml:"storage_namespace"`
	EventNamespace   string   `toml:"event_namespace"`
	DependsOn        []string `toml:"depends_on"`
}

type rawRouteBinding struct {
	Path        string `toml:"path"`
	Application string `toml:"application"`
	Instance    string `toml:"instance"`
}

var hostIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// LoadHost reads a pulp.host.toml and loads every referenced pulp.app.toml.
// Paths are relative to the host manifest. It rejects ambiguous routing,
// namespace sharing, invalid instance definitions, and application dependency
// cycles before a host can create any runtime instances.
func LoadHost(hostPath string) (*Host, error) {
	manifestPath, err := filepath.Abs(hostPath)
	if err != nil {
		return nil, fmt.Errorf("host manifest path: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read host manifest: %w", err)
	}

	var raw rawHost
	meta, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("parse host manifest: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) != 0 {
		names := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			names = append(names, key.String())
		}
		return nil, fmt.Errorf("unknown host manifest fields: %s", strings.Join(names, ", "))
	}

	schemaVersion := raw.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = CurrentHostSchemaVersion
	}
	if schemaVersion < 1 {
		return nil, fmt.Errorf("host schema_version must be >= 1 (got %d)", schemaVersion)
	}
	if schemaVersion > CurrentHostSchemaVersion {
		return nil, fmt.Errorf("host schema_version %d is newer than host supports (max %d)", schemaVersion, CurrentHostSchemaVersion)
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return nil, errors.New("host name is required")
	}
	if len(raw.Applications) == 0 {
		return nil, errors.New("host applications must contain at least one application")
	}

	baseDir := filepath.Dir(manifestPath)
	host := &Host{SchemaVersion: schemaVersion, Name: name, ManifestPath: manifestPath}
	byID := make(map[string]*HostedApplication, len(raw.Applications))
	storageOwners := make(map[string]string, len(raw.Applications))
	eventOwners := make(map[string]string, len(raw.Applications))
	for index, rawApp := range raw.Applications {
		app, err := loadHostedApplication(baseDir, index, rawApp)
		if err != nil {
			return nil, err
		}
		if _, exists := byID[app.ID]; exists {
			return nil, fmt.Errorf("duplicate application id %q", app.ID)
		}
		if owner, exists := storageOwners[app.StorageNamespace]; exists {
			return nil, fmt.Errorf("storage namespace %q is shared by applications %q and %q", app.StorageNamespace, owner, app.ID)
		}
		if owner, exists := eventOwners[app.EventNamespace]; exists {
			return nil, fmt.Errorf("event namespace %q is shared by applications %q and %q", app.EventNamespace, owner, app.ID)
		}
		storageOwners[app.StorageNamespace] = app.ID
		eventOwners[app.EventNamespace] = app.ID
		byID[app.ID] = app
		host.Applications = append(host.Applications, app)
	}
	applicationOrder, err := resolveApplicationOrder(host.Applications, byID)
	if err != nil {
		return nil, err
	}
	host.ApplicationOrder = applicationOrder
	if err := validateHostConsumes(host.Applications, byID); err != nil {
		return nil, err
	}

	routes, err := loadRouteBindings(raw.Routes, byID)
	if err != nil {
		return nil, err
	}
	host.Routes = routes
	return host, nil
}

// validateHostConsumes resolves cross-application grants only after the full
// host composition and its acyclic direct dependencies are known. Each exact
// provider must have one owning cell template across the caller's direct
// dependency applications; transitive, reverse, local, or globally visible
// providers never grant authority.
func validateHostConsumes(applications []*HostedApplication, byID map[string]*HostedApplication) error {
	for _, callerApp := range applications {
		providers := make(map[string][]string)
		for _, dependencyID := range callerApp.DependsOn {
			dependency := byID[dependencyID]
			if dependency == nil || dependency.Application == nil || dependency.Application.Cells == nil {
				continue
			}
			for _, targetCell := range dependency.Application.Cells.Cells {
				for _, provider := range targetCell.Provides {
					providers[provider] = append(providers[provider], dependency.ID+"/"+targetCell.Name)
				}
			}
		}
		if callerApp.Application == nil || callerApp.Application.Cells == nil {
			continue
		}
		for _, callerCell := range callerApp.Application.Cells.Cells {
			for _, provider := range callerCell.HostConsumes {
				owners := providers[provider]
				switch len(owners) {
				case 0:
					return fmt.Errorf("application %q cell %q host_consumes %q but no direct dependency application provides it", callerApp.ID, callerCell.Name, provider)
				case 1:
					// Exact and unique across direct dependencies.
				default:
					return fmt.Errorf("application %q cell %q host_consumes %q but direct dependencies provide it ambiguously: %s", callerApp.ID, callerCell.Name, provider, strings.Join(owners, ", "))
				}
			}
		}
	}
	return nil
}

func loadHostedApplication(baseDir string, index int, raw rawHostedApp) (*HostedApplication, error) {
	id := strings.TrimSpace(raw.ID)
	if !hostIdentifier.MatchString(id) {
		return nil, fmt.Errorf("applications[%d].id must match %s", index, hostIdentifier.String())
	}
	manifestPath, err := resolveHostRelativePath(baseDir, raw.Manifest, fmt.Sprintf("applications[%d].manifest", index))
	if err != nil {
		return nil, err
	}
	application, err := LoadApp(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("load application %q: %w", id, err)
	}
	instances, err := normalizeApplicationInstances(id, raw.Instances, raw.Aliases)
	if err != nil {
		return nil, fmt.Errorf("applications[%d]: %w", index, err)
	}
	storageNamespace, err := normalizeNamespace(raw.StorageNamespace, "storage_namespace")
	if err != nil {
		return nil, fmt.Errorf("applications[%d]: %w", index, err)
	}
	eventNamespace, err := normalizeNamespace(raw.EventNamespace, "event_namespace")
	if err != nil {
		return nil, fmt.Errorf("applications[%d]: %w", index, err)
	}
	return &HostedApplication{
		ID:               id,
		ManifestPath:     manifestPath,
		Application:      application,
		Instances:        instances,
		StorageNamespace: storageNamespace,
		EventNamespace:   eventNamespace,
		DependsOn:        normalizeHostedDependencies(raw.DependsOn),
	}, nil
}

func resolveHostRelativePath(baseDir, relative, field string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("%s must be relative to pulp.host.toml", field)
	}
	resolved, err := filepath.Abs(filepath.Join(baseDir, relative))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", field, err)
	}
	return resolved, nil
}

func normalizeApplicationInstances(id string, count int, aliases []string) ([]ApplicationInstance, error) {
	if count == 0 {
		count = 1
	}
	if count < 1 {
		return nil, errors.New("instances must be at least 1")
	}
	if len(aliases) > count {
		return nil, fmt.Errorf("aliases contains %d entries for %d instances", len(aliases), count)
	}
	instances := make([]ApplicationInstance, count)
	seen := make(map[string]struct{}, count)
	for ordinal := 1; ordinal <= count; ordinal++ {
		alias := ""
		if ordinal <= len(aliases) {
			alias = strings.TrimSpace(aliases[ordinal-1])
		}
		if alias == "" {
			alias = fmt.Sprintf("%s-%d", id, ordinal)
		}
		if !hostIdentifier.MatchString(alias) {
			return nil, fmt.Errorf("alias %q must match %s", alias, hostIdentifier.String())
		}
		if _, exists := seen[alias]; exists {
			return nil, fmt.Errorf("duplicate instance alias %q", alias)
		}
		seen[alias] = struct{}{}
		instances[ordinal-1] = ApplicationInstance{Ordinal: ordinal, Alias: alias}
	}
	return instances, nil
}

func normalizeNamespace(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if !hostIdentifier.MatchString(value) {
		return "", fmt.Errorf("%s must match %s", field, hostIdentifier.String())
	}
	return value, nil
}

func normalizeHostedDependencies(dependencies []string) []string {
	if len(dependencies) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(dependencies))
	result := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		dependency = strings.TrimSpace(dependency)
		if dependency == "" {
			continue
		}
		if _, exists := seen[dependency]; exists {
			continue
		}
		seen[dependency] = struct{}{}
		result = append(result, dependency)
	}
	return result
}

func resolveApplicationOrder(applications []*HostedApplication, byID map[string]*HostedApplication) ([]*HostedApplication, error) {
	for _, application := range applications {
		for _, dependency := range application.DependsOn {
			if _, exists := byID[dependency]; !exists {
				return nil, fmt.Errorf("application %q depends on unknown application %q", application.ID, dependency)
			}
		}
	}
	visiting := make(map[string]bool, len(applications))
	visited := make(map[string]bool, len(applications))
	stack := make([]string, 0, len(applications))
	order := make([]*HostedApplication, 0, len(applications))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			cycleStart := 0
			for i, item := range stack {
				if item == id {
					cycleStart = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[cycleStart:]...), id)
			return fmt.Errorf("application dependency cycle: %s", strings.Join(cycle, " -> "))
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		stack = append(stack, id)
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		visiting[id] = false
		visited[id] = true
		order = append(order, byID[id])
		return nil
	}
	for _, application := range applications {
		if err := visit(application.ID); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func loadRouteBindings(rawRoutes []rawRouteBinding, applications map[string]*HostedApplication) ([]*RouteBinding, error) {
	routes := make([]*RouteBinding, 0, len(rawRoutes))
	seenPaths := make(map[string]struct{}, len(rawRoutes))
	for index, rawRoute := range rawRoutes {
		routePath, err := normalizeRoutePath(rawRoute.Path)
		if err != nil {
			return nil, fmt.Errorf("routes[%d]: %w", index, err)
		}
		if _, exists := seenPaths[routePath]; exists {
			return nil, fmt.Errorf("duplicate route path %q", routePath)
		}
		applicationID := strings.TrimSpace(rawRoute.Application)
		application, exists := applications[applicationID]
		if !exists {
			return nil, fmt.Errorf("routes[%d] references unknown application %q", index, applicationID)
		}
		instance := strings.TrimSpace(rawRoute.Instance)
		if instance == "" {
			if len(application.Instances) != 1 {
				return nil, fmt.Errorf("routes[%d].instance is required for application %q with %d instances", index, applicationID, len(application.Instances))
			}
			instance = application.Instances[0].Alias
		}
		if !applicationHasInstance(application, instance) {
			return nil, fmt.Errorf("routes[%d] references unknown instance %q of application %q", index, instance, applicationID)
		}
		seenPaths[routePath] = struct{}{}
		routes = append(routes, &RouteBinding{Path: routePath, Application: applicationID, Instance: instance})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Path < routes[j].Path })
	return routes, nil
}

func applicationHasInstance(application *HostedApplication, alias string) bool {
	for _, instance := range application.Instances {
		if instance.Alias == alias {
			return true
		}
	}
	return false
}

func normalizeRoutePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("path is required")
	}
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#\\") {
		return "", fmt.Errorf("path %q must be an absolute URL path", value)
	}
	clean := path.Clean(value)
	if clean != value {
		return "", fmt.Errorf("path %q must be canonical", value)
	}
	return value, nil
}
