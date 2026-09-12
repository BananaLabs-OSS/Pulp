// Package product defines a presentation-neutral description of a product
// assembled around one or more Pulp applications. It belongs to Pulp rather than any
// particular workbench so products remain independently buildable.
package product

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const SchemaV1 = "pulp.product/v1"

var identityPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)

type Descriptor struct {
	Schema       string                 `json:"schema"`
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Version      string                 `json:"version"`
	Applications []Application          `json:"applications"`
	Entrypoint   Entrypoint             `json:"entrypoint"`
	Host         Host                   `json:"host"`
	Capabilities CapabilityRequirements `json:"capabilities"`
	Surfaces     []Surface              `json:"surfaces"`
	Integrations []string               `json:"optional_integrations,omitempty"`
}

type Application struct {
	ID           string   `json:"id"`
	Manifest     string   `json:"manifest"`
	Instance     string   `json:"instance,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type Entrypoint struct {
	Application string `json:"application"`
	Surface     string `json:"surface"`
	Path        string `json:"path,omitempty"`
}

type CapabilityRequirements struct {
	Required []string `json:"required,omitempty"`
	Optional []string `json:"optional,omitempty"`
}

type Host struct {
	Module     string   `json:"module"`
	Extensions []string `json:"extensions,omitempty"`
}

type Surface struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Root string `json:"root"`
}

type PlannedApplication struct {
	ID           string   `json:"id"`
	Manifest     string   `json:"manifest"`
	Instance     string   `json:"instance"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type Plan struct {
	Descriptor   string                 `json:"descriptor"`
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Version      string                 `json:"version"`
	Applications []PlannedApplication   `json:"applications"`
	Entrypoint   Entrypoint             `json:"entrypoint"`
	HostModule   string                 `json:"host_module"`
	Extensions   []string               `json:"extensions"`
	Capabilities CapabilityRequirements `json:"capabilities"`
	Surfaces     []Surface              `json:"surfaces"`
}

func Load(path string) (Descriptor, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, err
	}
	var descriptor Descriptor
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("decode product descriptor: %w", err)
	}
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	return descriptor, nil
}

func (d Descriptor) Validate() error {
	if d.Schema != SchemaV1 {
		return fmt.Errorf("product schema must be %q", SchemaV1)
	}
	if !identityPattern.MatchString(d.ID) {
		return errors.New("product id must be a lowercase dotted identity")
	}
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Version) == "" {
		return errors.New("product name and version are required")
	}
	if err := relativePath("host.module", d.Host.Module); err != nil {
		return err
	}
	if len(d.Applications) == 0 {
		return errors.New("at least one product application is required")
	}
	applications := map[string]bool{}
	for index, application := range d.Applications {
		if !identityPattern.MatchString(application.ID) || applications[application.ID] {
			return fmt.Errorf("applications[%d] has an invalid or duplicate id", index)
		}
		applications[application.ID] = true
		if err := relativePath(fmt.Sprintf("applications[%d].manifest", index), application.Manifest); err != nil {
			return err
		}
		if application.Instance != "" && !identityPattern.MatchString(application.Instance) {
			return fmt.Errorf("applications[%d].instance is invalid", index)
		}
	}
	for index, application := range d.Applications {
		seenDependency := map[string]bool{}
		for _, dependency := range application.Dependencies {
			if dependency == application.ID || !applications[dependency] || seenDependency[dependency] {
				return fmt.Errorf("applications[%d] has an invalid dependency %q", index, dependency)
			}
			seenDependency[dependency] = true
		}
	}
	if !applications[d.Entrypoint.Application] {
		return errors.New("entrypoint must select a declared application")
	}
	if len(d.Surfaces) == 0 {
		return errors.New("at least one product surface is required")
	}
	seen := map[string]bool{}
	for _, surface := range d.Surfaces {
		if !identityPattern.MatchString(surface.ID) || seen[surface.ID] {
			return fmt.Errorf("invalid or duplicate product surface %q", surface.ID)
		}
		switch surface.Kind {
		case "web", "tauri", "capacitor", "headless":
		default:
			return fmt.Errorf("unsupported product surface %q", surface.Kind)
		}
		seen[surface.ID] = true
		if surface.Kind != "headless" {
			if err := relativePath("surface root", surface.Root); err != nil {
				return err
			}
		}
	}
	if !seen[d.Entrypoint.Surface] {
		return errors.New("entrypoint must select a declared surface")
	}
	if d.Entrypoint.Path != "" && !strings.HasPrefix(d.Entrypoint.Path, "/") {
		return errors.New("entrypoint path must be absolute within its surface")
	}
	if err := validateCapabilities(d.Capabilities); err != nil {
		return err
	}
	for _, extension := range d.Host.Extensions {
		if !strings.Contains(extension, ".") || strings.ContainsAny(extension, " \t\r\n") {
			return fmt.Errorf("invalid host extension %q", extension)
		}
	}
	return nil
}

func Resolve(path string) (Plan, error) {
	descriptor, err := Load(path)
	if err != nil {
		return Plan{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Plan{}, err
	}
	root := filepath.Dir(abs)
	resolve := func(label, relative string) (string, error) {
		candidate := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
		within, err := filepath.Rel(root, candidate)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s escapes product root", label)
		}
		if _, err := os.Stat(candidate); err != nil {
			return "", fmt.Errorf("%s: %w", label, err)
		}
		return candidate, nil
	}
	applications := make([]PlannedApplication, 0, len(descriptor.Applications))
	for _, application := range descriptor.Applications {
		manifest, err := resolve("application "+application.ID, application.Manifest)
		if err != nil {
			return Plan{}, err
		}
		instance := application.Instance
		if instance == "" {
			instance = "default"
		}
		applications = append(applications, PlannedApplication{ID: application.ID, Manifest: manifest, Instance: instance, Dependencies: append([]string(nil), application.Dependencies...)})
	}
	hostModule, err := resolve("host.module", descriptor.Host.Module)
	if err != nil {
		return Plan{}, err
	}
	surfaces := append([]Surface(nil), descriptor.Surfaces...)
	for index := range surfaces {
		if surfaces[index].Kind == "headless" {
			continue
		}
		resolved, err := resolve("surface "+surfaces[index].Kind, surfaces[index].Root)
		if err != nil {
			return Plan{}, err
		}
		surfaces[index].Root = resolved
	}
	extensions := append([]string(nil), descriptor.Host.Extensions...)
	sort.Strings(extensions)
	return Plan{Descriptor: abs, ID: descriptor.ID, Name: descriptor.Name, Version: descriptor.Version,
		Applications: applications, Entrypoint: descriptor.Entrypoint, HostModule: hostModule, Extensions: extensions, Capabilities: descriptor.Capabilities, Surfaces: surfaces}, nil
}

func validateCapabilities(requirements CapabilityRequirements) error {
	seen := map[string]string{}
	for kind, values := range map[string][]string{"required": requirements.Required, "optional": requirements.Optional} {
		for _, value := range values {
			if !strings.Contains(value, ".") || strings.ContainsAny(value, " \t\r\n") {
				return fmt.Errorf("invalid %s capability %q", kind, value)
			}
			if previous := seen[value]; previous != "" {
				return fmt.Errorf("capability %q is both duplicate or declared as %s and %s", value, previous, kind)
			}
			seen[value] = kind
		}
	}
	return nil
}

func relativePath(label, value string) error {
	if value == "" || filepath.IsAbs(value) {
		return fmt.Errorf("%s must be a non-empty relative path", label)
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must stay inside the product root", label)
	}
	return nil
}
