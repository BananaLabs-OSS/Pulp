// Package product defines a presentation-neutral description of a product
// assembled around one Pulp application. It belongs to Pulp rather than any
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
	Schema       string    `json:"schema"`
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Application  string    `json:"application"`
	Host         Host      `json:"host"`
	Surfaces     []Surface `json:"surfaces"`
	Integrations []string  `json:"optional_integrations,omitempty"`
}

type Host struct {
	Module     string   `json:"module"`
	Extensions []string `json:"extensions,omitempty"`
}

type Surface struct {
	Kind string `json:"kind"`
	Root string `json:"root"`
}

type Plan struct {
	Descriptor  string    `json:"descriptor"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Application string    `json:"application"`
	HostModule  string    `json:"host_module"`
	Extensions  []string  `json:"extensions"`
	Surfaces    []Surface `json:"surfaces"`
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
	for label, path := range map[string]string{"application": d.Application, "host.module": d.Host.Module} {
		if err := relativePath(label, path); err != nil {
			return err
		}
	}
	if len(d.Surfaces) == 0 {
		return errors.New("at least one product surface is required")
	}
	seen := map[string]bool{}
	for _, surface := range d.Surfaces {
		switch surface.Kind {
		case "web", "tauri", "capacitor", "headless":
		default:
			return fmt.Errorf("unsupported product surface %q", surface.Kind)
		}
		if seen[surface.Kind] {
			return fmt.Errorf("duplicate product surface %q", surface.Kind)
		}
		seen[surface.Kind] = true
		if surface.Kind != "headless" {
			if err := relativePath("surface root", surface.Root); err != nil {
				return err
			}
		}
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
	application, err := resolve("application", descriptor.Application)
	if err != nil {
		return Plan{}, err
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
		Application: application, HostModule: hostModule, Extensions: extensions, Surfaces: surfaces}, nil
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
