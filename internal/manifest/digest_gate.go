package manifest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// CheckHostDigests validates the complete hosted application graph without
// changing it. LoadHost verifies Lua and Wasm pins as part of normal loading,
// making this the same fail-closed gate used at production startup.
func CheckHostDigests(path string) error {
	_, err := LoadHost(path)
	return err
}

// RefreshHostDigests deliberately refreshes all artifact pins reachable from a
// host manifest. Every update is computed and checked for conflicts before any
// file is replaced, preventing one malformed application from producing a
// partially refreshed graph. Individual replacements use fsync + rename.
func RefreshHostDigests(path string) error {
	hostPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	wire, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	var raw rawHost
	meta, err := toml.Decode(string(wire), &raw)
	if err != nil {
		return fmt.Errorf("parse host manifest: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) != 0 {
		return fmt.Errorf("host manifest contains unknown fields")
	}
	base := filepath.Dir(hostPath)
	byPath := map[string][]byte{}
	var ordered []digestUpdate
	for index, hosted := range raw.Applications {
		appPath, resolveErr := resolveAppRelativePath(base, hosted.Manifest, fmt.Sprintf("applications[%d].manifest", index))
		if resolveErr != nil {
			return resolveErr
		}
		planned, planErr := planAppDigestRefresh(appPath)
		if planErr != nil {
			return planErr
		}
		for _, update := range planned {
			if prior, exists := byPath[update.path]; exists {
				if !bytes.Equal(prior, update.data) {
					return fmt.Errorf("conflicting digest updates for %s", update.path)
				}
				continue
			}
			byPath[update.path] = update.data
			ordered = append(ordered, update)
		}
	}
	originals := make([]digestUpdate, 0, len(ordered))
	for _, update := range ordered {
		original, readErr := os.ReadFile(update.path)
		if readErr != nil {
			return readErr
		}
		originals = append(originals, digestUpdate{path: update.path, data: original})
	}
	if err := applyDigestUpdates(ordered); err != nil {
		return err
	}
	if err := CheckHostDigests(hostPath); err != nil {
		// Restore the exact pre-refresh bytes if graph validation discovers a
		// semantic error which digest planning alone cannot detect.
		for _, original := range originals {
			if rollbackErr := atomicReplace(original.path, original.data); rollbackErr != nil {
				return fmt.Errorf("refreshed digest graph is invalid (%v) and rollback of %s failed: %w", err, original.path, rollbackErr)
			}
		}
		return fmt.Errorf("refreshed digest graph is invalid: %w", err)
	}
	return nil
}
