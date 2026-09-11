package main

import (
	"bytes"
	"errors"

	pulp "github.com/BananaLabs-OSS/Fiber/pulp"
)

var state = []byte("initial")

func init() {
	pulp.OnSnapshot(func() ([]byte, error) {
		if bytes.Equal(state, []byte("snapshot-error")) {
			return nil, errors.New("fixture snapshot failed")
		}
		return state, nil
	})
	pulp.OnRestore(func(next []byte) error {
		if bytes.Equal(next, []byte("restore-error")) {
			return errors.New("fixture restore failed")
		}
		state = append(state[:0], next...)
		return nil
	})
}

func main() {}
