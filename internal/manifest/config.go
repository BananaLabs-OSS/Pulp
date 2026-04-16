package manifest

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// EncodeConfig serializes the manifest's [config] table to MessagePack bytes.
// The result is what the host passes to pulp_init as (config_ptr, config_len).
//
// Nil or empty configs encode to an empty byte slice. Callers must not assume
// a non-nil slice — pulp_init receives zero-length bytes when there is no
// config to deliver.
func EncodeConfig(config map[string]any) ([]byte, error) {
	if len(config) == 0 {
		return nil, nil
	}
	b, err := msgpack.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return b, nil
}
