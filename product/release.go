package product

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

const ReleaseStateSchemaV1 = "pulp.product-releases/v1"

type ReleaseState struct {
	Schema   string `json:"schema"`
	Active   string `json:"active,omitempty"`
	Previous string `json:"previous,omitempty"`
}

// InstallRelease verifies and content-addresses a frozen assembly. Existing
// release directories are immutable: identical content is idempotent and any
// mismatch is rejected.
func InstallRelease(assemblyRoot, storeRoot string) (string, error) {
	launch, err := loadFrozenLaunch(assemblyRoot)
	if err != nil {
		return "", err
	}
	if _, err := manifest.LoadHost(filepath.Join(assemblyRoot, launch.Host)); err != nil {
		return "", fmt.Errorf("verify frozen host: %w", err)
	}
	digest, files, err := treeDigest(assemblyRoot)
	if err != nil {
		return "", err
	}
	releases := filepath.Join(storeRoot, "releases")
	if err := os.MkdirAll(releases, 0o755); err != nil {
		return "", err
	}
	destination := filepath.Join(releases, digest)
	if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
		existing, _, digestErr := treeDigest(destination)
		if digestErr != nil || existing != digest {
			return "", errors.New("immutable release directory does not match its digest")
		}
		return digest, nil
	}
	temporary, err := os.MkdirTemp(releases, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	for _, relative := range files {
		if err := copyReleaseFile(assemblyRoot, temporary, relative); err != nil {
			return "", err
		}
	}
	if err := os.Rename(temporary, destination); err != nil {
		return "", fmt.Errorf("publish immutable release: %w", err)
	}
	return digest, nil
}

func ActivateRelease(storeRoot, digest string) (ReleaseState, error) {
	if !validDigest(digest) {
		return ReleaseState{}, errors.New("release digest must be lowercase SHA-256")
	}
	release := filepath.Join(storeRoot, "releases", digest)
	actual, _, err := treeDigest(release)
	if err != nil {
		return ReleaseState{}, err
	}
	if actual != digest {
		return ReleaseState{}, errors.New("release content does not match its immutable digest")
	}
	launch, err := loadFrozenLaunch(release)
	if err != nil {
		return ReleaseState{}, err
	}
	if _, err := manifest.LoadHost(filepath.Join(release, launch.Host)); err != nil {
		return ReleaseState{}, fmt.Errorf("verify release before activation: %w", err)
	}
	state, err := LoadReleaseState(storeRoot)
	if err != nil {
		return ReleaseState{}, err
	}
	if state.Active == digest {
		return state, nil
	}
	next := ReleaseState{Schema: ReleaseStateSchemaV1, Active: digest, Previous: state.Active}
	if err := writeReleaseState(storeRoot, next); err != nil {
		return ReleaseState{}, err
	}
	return next, nil
}

func RollbackRelease(storeRoot string) (ReleaseState, error) {
	state, err := LoadReleaseState(storeRoot)
	if err != nil {
		return ReleaseState{}, err
	}
	if state.Previous == "" {
		return ReleaseState{}, errors.New("no previous product release")
	}
	previous, active := state.Previous, state.Active
	next, err := ActivateRelease(storeRoot, previous)
	if err != nil {
		return ReleaseState{}, err
	}
	next.Previous = active
	if err := writeReleaseState(storeRoot, next); err != nil {
		return ReleaseState{}, err
	}
	return next, nil
}

func LoadReleaseState(storeRoot string) (ReleaseState, error) {
	body, err := os.ReadFile(filepath.Join(storeRoot, "current.json"))
	if os.IsNotExist(err) {
		return ReleaseState{Schema: ReleaseStateSchemaV1}, nil
	}
	if err != nil {
		return ReleaseState{}, err
	}
	var state ReleaseState
	if err := json.Unmarshal(body, &state); err != nil || state.Schema != ReleaseStateSchemaV1 || (state.Active != "" && !validDigest(state.Active)) || (state.Previous != "" && !validDigest(state.Previous)) {
		return ReleaseState{}, errors.New("invalid product release state")
	}
	return state, nil
}

func loadFrozenLaunch(root string) (LaunchContract, error) {
	body, err := os.ReadFile(filepath.Join(root, "pulp.product.launch.json"))
	if err != nil {
		return LaunchContract{}, fmt.Errorf("read frozen launch contract: %w", err)
	}
	var launch LaunchContract
	if err := json.Unmarshal(body, &launch); err != nil {
		return LaunchContract{}, errors.New("invalid frozen launch contract")
	}
	cleanHost := filepath.Clean(filepath.FromSlash(launch.Host))
	if launch.Schema != LaunchSchemaV1 || launch.Mode != "frozen" || launch.Host == "" || filepath.IsAbs(launch.Host) || cleanHost == ".." || strings.HasPrefix(cleanHost, ".."+string(filepath.Separator)) {
		return LaunchContract{}, errors.New("invalid frozen launch contract")
	}
	return launch, nil
}

func treeDigest(root string) (string, []string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("release contains non-regular file %q", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, relative := range files {
		io.WriteString(hash, fmt.Sprintf("%d:%s", len(relative), relative))
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", nil, err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", nil, copyErr
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), files, nil
}

func copyReleaseFile(sourceRoot, destinationRoot, relative string) error {
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	destination := filepath.Join(destinationRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destination, body, info.Mode().Perm())
}

func writeReleaseState(storeRoot string, state ReleaseState) error {
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(storeRoot, "current.json"), append(body, '\n'), 0o644)
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
