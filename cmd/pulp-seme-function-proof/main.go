// pulp-seme-function-proof exercises a generated Seme pure-function cell
// through Pulp's normal manifest, lifecycle, allocator, and provider-call
// path. It deliberately knows only provider names and opaque request/response
// bytes; semantic signatures and codecs remain owned by Seme's generated cell.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
)

func init() {
	ext.Register(ext.Capability{Name: "observability.log", Provider: "seme.pulp.log-v1", Register: bindLog, Stub: bindLogStub})
}

func bindLog(builder wazero.HostModuleBuilder, cell ext.Cell) error {
	builder.NewFunctionBuilder().WithFunc(func(_ context.Context, value uint32) uint32 {
		fmt.Printf("[observability.log] cell=%s value=%t\n", cell.Name(), value != 0)
		return 0
	}).Export("log_bool")
	return nil
}

func bindLogStub(builder wazero.HostModuleBuilder, _ ext.Cell) error {
	builder.NewFunctionBuilder().WithFunc(func(context.Context, uint32) uint32 { return 99 }).Export("log_bool")
	return nil
}

type options struct {
	manifest string
	provider string
	requests [][]byte
}

func main() {
	options, err := parseArgs(os.Args[1:])
	if err == nil {
		err = call(options)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pulp-seme-function-proof:", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (options, error) {
	var result options
	for index := 0; index < len(args); index++ {
		name, value, found := strings.Cut(args[index], "=")
		if !found {
			if index+1 >= len(args) {
				return result, fmt.Errorf("%s requires a value", name)
			}
			index++
			value = args[index]
		}
		switch name {
		case "-manifest":
			result.manifest = value
		case "-provider":
			result.provider = value
		case "-request":
			decoded, err := hex.DecodeString(value)
			if err != nil {
				return result, fmt.Errorf("request must be hexadecimal: %w", err)
			}
			result.requests = append(result.requests, decoded)
		default:
			return result, fmt.Errorf("unsupported argument %s", name)
		}
	}
	if result.manifest == "" || result.provider == "" || len(result.requests) == 0 {
		return result, fmt.Errorf("requires -manifest, -provider, and at least one -request")
	}
	return result, nil
}

func call(options options) error {
	spec, err := manifest.Load(options.manifest)
	if err != nil {
		return err
	}
	declared := false
	for _, provider := range spec.Provides {
		if provider == options.provider {
			declared = true
			break
		}
	}
	if !declared {
		return fmt.Errorf("cell %q does not provide %q", spec.Name, options.provider)
	}
	ctx := context.Background()
	cell, err := host.Load(ctx, spec, host.NewRegistry(), &host.Limits{}, slog.Default())
	if err != nil {
		return err
	}
	defer cell.Close(ctx)
	if err := cell.Init(ctx, nil); err != nil {
		return err
	}
	defer cell.Shutdown(ctx)
	encoder := json.NewEncoder(os.Stdout)
	for _, request := range options.requests {
		response, err := cell.Call(ctx, options.provider, request)
		if err != nil {
			return err
		}
		if err := encoder.Encode(struct {
			Request  string `json:"request"`
			Response string `json:"response"`
		}{hex.EncodeToString(request), hex.EncodeToString(response)}); err != nil {
			return err
		}
	}
	return nil
}
