// pulp-seme-proof is the deployment binary for Seme's first real Pulp target
// conformance proof. It adds one capability-shaped logging host import to the
// normal Pulp runtime without changing Pulp core.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/run"
	"github.com/tetratelabs/wazero"
)

func init() {
	ext.Register(ext.Capability{
		Name:     "observability.log",
		Provider: "seme.pulp.log-v1",
		Register: bindLog,
		Stub:     bindLogStub,
	})
}

func bindLog(builder wazero.HostModuleBuilder, cell ext.Cell) error {
	builder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, value uint32) uint32 {
			fmt.Printf("[observability.log] cell=%s quota.accepted=%t\n", cell.Name(), value != 0)
			return 0
		}).
		Export("log_bool")
	return nil
}

func bindLogStub(builder wazero.HostModuleBuilder, _ ext.Cell) error {
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, uint32) uint32 { return 99 }).
		Export("log_bool")
	return nil
}

func main() {
	manifestPath, requests, applicationMode, parseErr := applicationArgs(os.Args[1:])
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, "pulp-seme-proof:", parseErr)
		os.Exit(2)
	}
	if applicationMode {
		if err := callRequests(manifestPath, requests); err != nil {
			fmt.Fprintln(os.Stderr, "pulp-seme-proof:", err)
			os.Exit(1)
		}
		return
	}
	run.Main()
}

type quotaRequest struct {
	Current int64 `json:"current"`
	Delta   int64 `json:"delta"`
	Limit   int64 `json:"limit"`
}
type requestFlags []quotaRequest

func (r *requestFlags) add(value string) error {
	parts := strings.Split(value, ",")
	if len(parts) != 3 {
		return fmt.Errorf("request must contain CURRENT,DELTA,LIMIT")
	}
	values := [3]int64{}
	for index := range parts {
		parsed, err := strconv.ParseInt(parts[index], 10, 64)
		if err != nil {
			return fmt.Errorf("request field %d: %w", index, err)
		}
		values[index] = parsed
	}
	*r = append(*r, quotaRequest{values[0], values[1], values[2]})
	return nil
}

func applicationArgs(args []string) (string, requestFlags, bool, error) {
	applicationMode := false
	for _, arg := range args {
		if arg == "-request" || strings.HasPrefix(arg, "-request=") {
			applicationMode = true
		}
	}
	if !applicationMode {
		return "", nil, false, nil
	}
	var manifestPath string
	var requests requestFlags
	for index := 0; index < len(args); index++ {
		name, value, found := strings.Cut(args[index], "=")
		if !found {
			if index+1 >= len(args) {
				return "", nil, true, fmt.Errorf("%s requires a value", name)
			}
			index++
			value = args[index]
		}
		switch name {
		case "-manifest":
			manifestPath = value
		case "-request":
			if err := requests.add(value); err != nil {
				return "", nil, true, err
			}
		default:
			return "", nil, true, fmt.Errorf("unsupported application argument %s", name)
		}
	}
	if manifestPath == "" || len(requests) == 0 {
		return "", nil, true, fmt.Errorf("application mode requires -manifest and at least one -request")
	}
	return manifestPath, requests, true, nil
}

func callRequests(manifestPath string, requests []quotaRequest) error {
	spec, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	providerDeclared := false
	for _, provider := range spec.Provides {
		if provider == "quota.admit-v1" {
			providerDeclared = true
			break
		}
	}
	if !providerDeclared {
		return fmt.Errorf("cell %q does not provide quota.admit-v1", spec.Name)
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
	for _, request := range requests {
		wire := make([]byte, 24)
		binary.LittleEndian.PutUint64(wire[0:8], uint64(request.Current))
		binary.LittleEndian.PutUint64(wire[8:16], uint64(request.Delta))
		binary.LittleEndian.PutUint64(wire[16:24], uint64(request.Limit))
		response, err := cell.Call(ctx, "quota.admit-v1", wire)
		if err != nil {
			return err
		}
		if len(response) != 1 || response[0] > 1 {
			return fmt.Errorf("invalid quota response: %x", response)
		}
		if err := encoder.Encode(struct {
			Request  quotaRequest `json:"request"`
			Accepted bool         `json:"accepted"`
		}{request, response[0] == 1}); err != nil {
			return err
		}
	}
	return nil
}
