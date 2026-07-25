package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// normalizeSHA256 validates a SHA-256 digest declared in a manifest and
// returns its canonical lower-case hexadecimal form.
func normalizeSHA256(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%s must be exactly 64 hexadecimal characters", field)
	}
	return hex.EncodeToString(decoded), nil
}

// verifyWASMDigest validates a cell package artifact when its manifest pins
// one. App manifests can additionally require a pin for every cell.
func verifyWASMDigest(spec *CellSpec, required bool) error {
	if spec.WASMSHA256 == "" {
		if required {
			return errors.New("wasm_sha256 is required by require_wasm_sha256")
		}
		return nil
	}
	wasm, err := os.ReadFile(spec.WASMPath)
	if err != nil {
		return fmt.Errorf("read wasm: %w", err)
	}
	actual := sha256.Sum256(wasm)
	if hex.EncodeToString(actual[:]) != spec.WASMSHA256 {
		return fmt.Errorf("WASM SHA-256 mismatch: got %x, want %s", actual, spec.WASMSHA256)
	}
	return nil
}
