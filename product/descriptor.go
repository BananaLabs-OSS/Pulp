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

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
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
	Integrations []string               `json:"optional_integrations,omitempty"`
}

const LaunchSchemaV1 = "pulp.product-launch/v1"

// LaunchContract is consumed by focused web, desktop, mobile, and headless
// shells. It contains no shell-specific behavior: the shell starts Host,
// waits for HealthPath, and opens EntrypointPath when it has a visual surface.
type LaunchContract struct {
	Schema         string                 `json:"schema"`
	Product        string                 `json:"product"`
	Name           string                 `json:"name"`
	Version        string                 `json:"version"`
	Mode           string                 `json:"mode"`
	Host           string                 `json:"host"`
	HealthPath     string                 `json:"health_path"`
	EntrypointPath string                 `json:"entrypoint_path"`
	Application    string                 `json:"application"`
	Surface        Surface                `json:"surface"`
	Capabilities   CapabilityRequirements `json:"capabilities"`
	Integrations   []string               `json:"optional_integrations,omitempty"`
}

// Assembly is the deterministic, runnable output produced from a product
// plan. The generated host manifest delegates execution to Pulp's ordinary
// multi-application supervisor; products do not introduce a second runtime.
type Assembly struct {
	Root           string  `json:"root"`
	HostManifest   string  `json:"host_manifest"`
	PlanManifest   string  `json:"plan_manifest"`
	LaunchManifest string  `json:"launch_manifest"`
	Surface        Surface `json:"surface"`
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
	available := make(map[string]bool, len(extensions))
	for _, extension := range extensions {
		available[extension] = true
	}
	for _, required := range descriptor.Capabilities.Required {
		if !available[required] {
			return Plan{}, fmt.Errorf("required capability %q has no host extension", required)
		}
	}
	return Plan{Descriptor: abs, ID: descriptor.ID, Name: descriptor.Name, Version: descriptor.Version,
		Applications: applications, Entrypoint: descriptor.Entrypoint, HostModule: hostModule, Extensions: extensions, Capabilities: descriptor.Capabilities, Surfaces: surfaces,
		Integrations: append([]string(nil), descriptor.Integrations...)}, nil
}

// Assemble writes a portable host composition and resolved plan. Output is
// replaced file-by-file so an interrupted assembly never leaves partial file
// contents. Source application manifests remain the authority and are
// referenced relatively, which keeps development builds inspectable.
func Assemble(descriptorPath, outputRoot, surfaceID string) (Assembly, error) {
	return assemble(descriptorPath, outputRoot, surfaceID, false)
}

// AssembleFrozen copies the exact verified composition inputs into the output
// tree. The resulting host manifest has no references back to the source
// checkout and starts without a registry or network connection.
func AssembleFrozen(descriptorPath, outputRoot, surfaceID string) (Assembly, error) {
	return assemble(descriptorPath, outputRoot, surfaceID, true)
}

func assemble(descriptorPath, outputRoot, surfaceID string, frozen bool) (Assembly, error) {
	plan, err := Resolve(descriptorPath)
	if err != nil {
		return Assembly{}, err
	}
	var selected *Surface
	for index := range plan.Surfaces {
		if plan.Surfaces[index].ID == surfaceID {
			selected = &plan.Surfaces[index]
			break
		}
	}
	if selected == nil {
		return Assembly{}, fmt.Errorf("product surface %q is not declared", surfaceID)
	}
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return Assembly{}, fmt.Errorf("assembly root: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Assembly{}, fmt.Errorf("create assembly root: %w", err)
	}
	mode := "linked"
	if frozen {
		mode = "frozen"
		if err := freezeApplications(&plan, root); err != nil {
			return Assembly{}, err
		}
		if err := copyProductFile(filepath.Dir(plan.Descriptor), root, plan.Descriptor); err != nil {
			return Assembly{}, fmt.Errorf("freeze product descriptor: %w", err)
		}
		if selected.Kind != "headless" {
			relativeSurface := filepath.Join("surface", selected.ID)
			if err := copyProductTree(selected.Root, filepath.Join(root, relativeSurface)); err != nil {
				return Assembly{}, fmt.Errorf("freeze surface %q: %w", selected.ID, err)
			}
			selected.Root = filepath.ToSlash(relativeSurface)
		}
	}
	hostPath := filepath.Join(root, "pulp.host.toml")
	planPath := filepath.Join(root, "pulp.product.plan.json")
	launchPath := filepath.Join(root, "pulp.product.launch.json")
	hostBody, err := renderHost(plan, root)
	if err != nil {
		return Assembly{}, err
	}
	recordedPlan := plan
	recordedPlan.Applications = append([]PlannedApplication(nil), plan.Applications...)
	recordedPlan.Surfaces = append([]Surface(nil), plan.Surfaces...)
	launchSurface := *selected
	if frozen {
		recordedPlan.Descriptor = "pulp.product.json"
		recordedPlan.HostModule = ""
		for index := range recordedPlan.Applications {
			relative, relErr := filepath.Rel(root, recordedPlan.Applications[index].Manifest)
			if relErr != nil {
				return Assembly{}, relErr
			}
			recordedPlan.Applications[index].Manifest = filepath.ToSlash(relative)
		}
		for index := range recordedPlan.Surfaces {
			if recordedPlan.Surfaces[index].ID == selected.ID {
				recordedPlan.Surfaces[index].Root = selected.Root
			} else {
				recordedPlan.Surfaces[index].Root = ""
			}
		}
		launchSurface.Root = selected.Root
	}
	planBody, err := json.MarshalIndent(recordedPlan, "", "  ")
	if err != nil {
		return Assembly{}, fmt.Errorf("encode product plan: %w", err)
	}
	planBody = append(planBody, '\n')
	entrypointPath := plan.Entrypoint.Path
	if entrypointPath == "" {
		entrypointPath = "/"
	}
	launch := LaunchContract{Schema: LaunchSchemaV1, Product: plan.ID, Name: plan.Name, Version: plan.Version, Mode: mode,
		Host: filepath.Base(hostPath), HealthPath: "/_pulp/health", EntrypointPath: entrypointPath, Application: plan.Entrypoint.Application,
		Surface: launchSurface, Capabilities: plan.Capabilities, Integrations: append([]string(nil), plan.Integrations...)}
	launchBody, err := json.MarshalIndent(launch, "", "  ")
	if err != nil {
		return Assembly{}, fmt.Errorf("encode launch contract: %w", err)
	}
	launchBody = append(launchBody, '\n')
	if err := atomicWriteValidated(hostPath, hostBody, 0o644, func(candidate string) error {
		_, err := manifest.LoadHost(candidate)
		return err
	}); err != nil {
		return Assembly{}, err
	}
	if err := atomicWrite(planPath, planBody, 0o644); err != nil {
		return Assembly{}, err
	}
	if err := atomicWrite(launchPath, launchBody, 0o644); err != nil {
		return Assembly{}, err
	}
	return Assembly{Root: root, HostManifest: hostPath, PlanManifest: planPath, LaunchManifest: launchPath, Surface: launchSurface}, nil
}

func copyProductTree(sourceRoot, destinationRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("surface contains symlink %q", relative)
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destinationRoot, relative), 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("surface contains non-regular file %q", relative)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return atomicWrite(filepath.Join(destinationRoot, relative), body, info.Mode().Perm())
	})
}

func freezeApplications(plan *Plan, outputRoot string) error {
	type frozenApplication struct {
		manifest string
		paths    []string
	}
	frozen := make([]frozenApplication, len(plan.Applications))
	allPaths := []string{filepath.Dir(plan.Descriptor)}
	for index := range plan.Applications {
		application, err := manifest.LoadApp(plan.Applications[index].Manifest)
		if err != nil {
			return fmt.Errorf("freeze application %q: %w", plan.Applications[index].ID, err)
		}
		paths := []string{application.ManifestPath, application.OrchestrationScript}
		for _, cell := range application.Cells.Cells {
			paths = append(paths, cell.ManifestPath, cell.WASMPath)
		}
		for _, unit := range application.ExecutionUnits {
			if unit.Artifact != nil {
				paths = append(paths, unit.Artifact.ManifestPath, unit.Artifact.WASMPath)
			}
		}
		frozen[index] = frozenApplication{manifest: application.ManifestPath, paths: paths}
		allPaths = append(allPaths, paths...)
	}
	sourceRoot, err := commonDirectory(allPaths)
	if err != nil {
		return err
	}
	packageRoot := filepath.Join(outputRoot, "packages")
	for index, application := range frozen {
		for _, source := range application.paths {
			if err := copyProductFile(sourceRoot, packageRoot, source); err != nil {
				return fmt.Errorf("freeze application %q: %w", plan.Applications[index].ID, err)
			}
		}
		relative, err := filepath.Rel(sourceRoot, application.manifest)
		if err != nil {
			return err
		}
		plan.Applications[index].Manifest = filepath.Join(packageRoot, relative)
	}
	return nil
}

func commonDirectory(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("composition contains no source paths")
	}
	root, err := filepath.Abs(paths[0])
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(root); statErr == nil && !info.IsDir() {
		root = filepath.Dir(root)
	}
	for _, candidate := range paths[1:] {
		candidate, err = filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
		for {
			relative, relErr := filepath.Rel(root, candidate)
			if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return "", errors.New("composition paths do not share a filesystem root")
			}
			root = parent
		}
	}
	return root, nil
}

func copyProductFile(sourceRoot, outputRoot, source string) error {
	relative, err := filepath.Rel(sourceRoot, source)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("composition file %q escapes product root", source)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("composition file %q is not a regular file", source)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	destination := filepath.Join(outputRoot, relative)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return atomicWrite(destination, body, info.Mode().Perm())
}

func renderHost(plan Plan, outputRoot string) ([]byte, error) {
	var body strings.Builder
	fmt.Fprintf(&body, "schema_version = 1\nname = %q\nhealth_path = %q\n", plan.ID, "/_pulp/health")
	for _, application := range plan.Applications {
		relative, err := filepath.Rel(outputRoot, application.Manifest)
		if err != nil {
			return nil, fmt.Errorf("relativize application %q: %w", application.ID, err)
		}
		fmt.Fprintf(&body, "\n[[applications]]\nid = %q\nmanifest = %q\naliases = [%q]\nstorage_namespace = %q\nevent_namespace = %q\n",
			application.ID, filepath.ToSlash(relative), application.Instance, application.ID, application.ID)
		if len(application.Dependencies) != 0 {
			fmt.Fprintf(&body, "depends_on = %s\n", quotedList(application.Dependencies))
		}
	}
	entryInstance := "default"
	for _, application := range plan.Applications {
		if application.ID == plan.Entrypoint.Application {
			entryInstance = application.Instance
			break
		}
	}
	fmt.Fprintf(&body, "\n[[routes]]\npath = %q\napplication = %q\ninstance = %q\n", "/", plan.Entrypoint.Application, entryInstance)
	return []byte(body.String()), nil
}

func quotedList(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = fmt.Sprintf("%q", value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	return atomicWriteValidated(path, body, mode, nil)
}

func atomicWriteValidated(path string, body []byte, mode os.FileMode, validate func(string) error) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pulp-product-*")
	if err != nil {
		return fmt.Errorf("create temporary product file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(temporaryPath); err != nil {
			return fmt.Errorf("validate assembled product: %w", err)
		}
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish product file: %w", err)
	}
	return nil
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
