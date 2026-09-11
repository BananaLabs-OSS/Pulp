package manifest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

// RefreshAppDigests replaces the legacy app Lua digest and every referenced
// cell Wasm digest from the bytes currently on disk. Files are prepared first
// and individually replaced atomically, so malformed input changes nothing.
func RefreshAppDigests(path string) error {
	updates, err := planAppDigestRefresh(path)
	if err != nil {
		return err
	}
	return applyDigestUpdates(updates)
}

// RefreshCellDigest replaces one cell manifest's Wasm digest with the digest
// of the artifact currently referenced by that manifest. Validation happens
// before the manifest is atomically replaced.
func RefreshCellDigest(path string) error {
	cellPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	cellBytes, err := os.ReadFile(cellPath)
	if err != nil {
		return err
	}
	var raw struct {
		Wasm string `toml:"wasm"`
	}
	if _, err = toml.Decode(string(cellBytes), &raw); err != nil {
		return fmt.Errorf("parse cell manifest: %w", err)
	}
	wasmPath, err := resolveAppRelativePath(filepath.Dir(cellPath), raw.Wasm, "wasm")
	if err != nil {
		return err
	}
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return err
	}
	updated, err := replaceTopLevelValue(cellBytes, "wasm_sha256", fmt.Sprintf("%x", sha256.Sum256(wasm)))
	if err != nil {
		return err
	}
	return applyDigestUpdates([]digestUpdate{{path: cellPath, data: updated}})
}

type digestUpdate struct {
	path string
	data []byte
}

func planAppDigestRefresh(path string) ([]digestUpdate, error) {
	appPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	appBytes, err := os.ReadFile(appPath)
	if err != nil {
		return nil, err
	}
	var raw rawApplication
	if _, err = toml.Decode(string(appBytes), &raw); err != nil {
		return nil, fmt.Errorf("parse app manifest: %w", err)
	}
	base := filepath.Dir(appPath)
	scriptPath, err := resolveAppRelativePath(base, raw.Orchestrator.Script, "orchestrator.script")
	if err != nil {
		return nil, err
	}
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, err
	}
	updatedApp, err := replaceSectionValue(appBytes, "orchestrator", "sha256", fmt.Sprintf("%x", sha256.Sum256(script)))
	if err != nil {
		return nil, err
	}
	updates := []digestUpdate{{appPath, updatedApp}}
	for index, relative := range raw.Cells {
		cellPath, resolveErr := resolveAppRelativePath(base, relative, fmt.Sprintf("cells[%d]", index))
		if resolveErr != nil {
			return nil, resolveErr
		}
		cellBytes, readErr := os.ReadFile(cellPath)
		if readErr != nil {
			return nil, readErr
		}
		var cellRaw struct {
			Wasm string `toml:"wasm"`
		}
		if _, readErr = toml.Decode(string(cellBytes), &cellRaw); readErr != nil {
			return nil, fmt.Errorf("parse cell manifest %s: %w", cellPath, readErr)
		}
		wasmPath, resolveErr := resolveAppRelativePath(filepath.Dir(cellPath), cellRaw.Wasm, "wasm")
		if resolveErr != nil {
			return nil, resolveErr
		}
		wasm, readErr := os.ReadFile(wasmPath)
		if readErr != nil {
			return nil, readErr
		}
		updated, replaceErr := replaceTopLevelValue(cellBytes, "wasm_sha256", fmt.Sprintf("%x", sha256.Sum256(wasm)))
		if replaceErr != nil {
			return nil, replaceErr
		}
		updates = append(updates, digestUpdate{cellPath, updated})
	}
	return updates, nil
}

func applyDigestUpdates(updates []digestUpdate) error {
	originals := make([]digestUpdate, 0, len(updates))
	for _, item := range updates {
		original, err := os.ReadFile(item.path)
		if err != nil {
			return err
		}
		originals = append(originals, digestUpdate{path: item.path, data: original})
	}
	for index, item := range updates {
		if err := atomicReplace(item.path, item.data); err != nil {
			for rollback := index - 1; rollback >= 0; rollback-- {
				_ = atomicReplace(originals[rollback].path, originals[rollback].data)
			}
			return fmt.Errorf("replace %s (prior updates rolled back): %w", item.path, err)
		}
	}
	return nil
}

func replaceSectionValue(data []byte, section, key, value string) ([]byte, error) {
	sectionPattern := regexp.MustCompile(`(?m)^\s*\[` + regexp.QuoteMeta(section) + `\]\s*$`)
	location := sectionPattern.FindIndex(data)
	if location == nil {
		return nil, fmt.Errorf("missing [%s] section", section)
	}
	end := len(data)
	if next := regexp.MustCompile(`(?m)^\s*\[`).FindIndex(data[location[1]:]); next != nil {
		end = location[1] + next[0]
	}
	block := data[location[1]:end]
	keyPattern := regexp.MustCompile(`(?m)^(\s*` + regexp.QuoteMeta(key) + `\s*=\s*)[^\r\n]*(\r?)$`)
	replacement := []byte(`${1}"` + value + `"${2}`)
	if keyPattern.Match(block) {
		block = keyPattern.ReplaceAll(block, replacement)
	} else {
		// block aliases data. Allocate before appending so growth cannot overwrite
		// the following section that is copied into the result below.
		block = append(append([]byte(nil), block...), []byte("\n"+key+" = \""+value+"\"\n")...)
	}
	return append(append(append([]byte(nil), data[:location[1]]...), block...), data[end:]...), nil
}

func replaceTopLevelValue(data []byte, key, value string) ([]byte, error) {
	end := len(data)
	if section := regexp.MustCompile(`(?m)^\s*\[`).FindIndex(data); section != nil {
		end = section[0]
	}
	head := data[:end]
	pattern := regexp.MustCompile(`(?m)^(\s*` + regexp.QuoteMeta(key) + `\s*=\s*)[^\r\n]*(\r?)$`)
	if pattern.Match(head) {
		head = pattern.ReplaceAll(head, []byte(`${1}"`+value+`"${2}`))
	} else {
		// head aliases data. Allocate before appending so a manifest with a
		// following [config] section cannot be overwritten through slice capacity.
		head = append(append([]byte(nil), head...), []byte(key+" = \""+value+"\"\n")...)
	}
	return append(append([]byte(nil), head...), data[end:]...), nil
}

func atomicReplace(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pulp-refresh-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(name, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(name, path)
}
