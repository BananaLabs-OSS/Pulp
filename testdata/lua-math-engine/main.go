package main

import (
	"fmt"

	"github.com/BananaLabs-OSS/Fiber/pulp"
	"github.com/vmihailenco/msgpack/v5"
)

func init() {
	pulp.OnInit(func([]byte) error {
		pulp.Provide("math.double", func(input []byte) ([]byte, error) {
			var request struct {
				Value int64 `msgpack:"value"`
			}
			if err := msgpack.Unmarshal(input, &request); err != nil {
				return nil, fmt.Errorf("decode: %w", err)
			}
			return msgpack.Marshal(map[string]any{"value": request.Value * 2})
		})
		return nil
	})
}

func main() {}
