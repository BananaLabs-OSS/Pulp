package run

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/vmihailenco/msgpack/v5"
)

type luaDispatchRequest struct {
	Event   string `msgpack:"event"`
	Payload any    `msgpack:"payload,omitempty"`
}

type luaAction struct {
	Name    string `msgpack:"name"`
	Payload any    `msgpack:"payload,omitempty"`
}

type luaDispatchResult struct {
	Value    any         `msgpack:"value,omitempty"`
	Commands []luaAction `msgpack:"commands,omitempty"`
	Events   []luaAction `msgpack:"events,omitempty"`
}

func buildLuaHarnessCell(t *testing.T, sourceDir, outputName, goCache string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), outputName+".wasm")
	command := exec.Command("go", "build", "-buildvcs=false", "-buildmode=c-shared", "-o", output, ".")
	command.Dir = sourceDir
	command.Env = append(os.Environ(),
		"GOOS=wasip1",
		"GOARCH=wasm",
		"GOCACHE="+goCache,
	)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", sourceDir, err, combined)
	}
	return output
}

func TestLuaOrchestratorCoordinatesSiblingEngines(t *testing.T) {
	cache := t.TempDir()
	mathWASM := buildLuaHarnessCell(t, filepath.Join("..", "testdata", "lua-math-engine"), "math-engine", cache)
	textWASM := buildLuaHarnessCell(t, filepath.Join("..", "testdata", "lua-text-engine"), "text-engine", cache)
	luaWASM := buildLuaHarnessCell(t, filepath.Join("..", "..", "Pulp-Lua", "pulp-cell"), "lua-orchestrator", cache)

	script := `
pulp.on("compose", function(input)
  local doubled = pulp.call("math-engine", "math.double", { value = input.value })
  local labeled = pulp.call("text-engine", "text.label", {
    prefix = input.prefix,
    value = doubled.value
  })
  local runs = (pulp.state_get("runs") or 0) + 1
  pulp.state_set("runs", runs)
  pulp.command("render.text", { text = labeled.text })
  pulp.emit("composition.completed", { runs = runs })
  return { text = labeled.text, runs = runs }
end)
`

	specs := map[string]*manifest.CellSpec{
		"math-engine": {
			Name:     "math-engine",
			Version:  "0.0.0-test",
			WASMPath: mathWASM,
			Provides: []string{"math.double"},
		},
		"text-engine": {
			Name:     "text-engine",
			Version:  "0.0.0-test",
			WASMPath: textWASM,
			Provides: []string{"text.label"},
		},
		"lua-orchestrator": {
			Name:      "lua-orchestrator",
			Version:   "0.0.0-test",
			WASMPath:  luaWASM,
			Provides:  []string{"orchestrator"},
			Consumes:  []string{"math.double", "text.label"},
			DependsOn: []string{"math-engine", "text-engine"},
			Config: map[string]any{
				"script":     script,
				"timeout_ms": int64(5000),
			},
		},
	}

	runtimes := map[string]*cellRuntime{}
	for name, spec := range specs {
		runtimes[name] = &cellRuntime{spec: spec}
	}
	registry := host.NewRegistry()
	registry.Always(siblingCapability(newSiblingRegistry(runtimes)))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, name := range []string{"math-engine", "text-engine", "lua-orchestrator"} {
		spec := specs[name]
		cell, err := host.Load(ctx, spec, registry, nil, logger)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		runtimes[name].cell = cell
		config, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			t.Fatalf("encode %s config: %v", name, err)
		}
		if err := cell.Init(ctx, config); err != nil {
			t.Fatalf("init %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = cell.Shutdown(context.Background())
			_ = cell.Close(context.Background())
		})
	}

	orchestratorCell := runtimes["lua-orchestrator"].cell
	for attempt, wantRuns := range []int64{1, 2} {
		request, err := msgpack.Marshal(luaDispatchRequest{
			Event: "compose",
			Payload: map[string]any{
				"value":  int64(21),
				"prefix": "answer",
			},
		})
		if err != nil {
			t.Fatalf("marshal dispatch: %v", err)
		}
		response, err := orchestratorCell.Call(ctx, "orchestrator.dispatch", request)
		if err != nil {
			t.Fatalf("dispatch %d: %v", attempt, err)
		}
		var result luaDispatchResult
		if err := msgpack.Unmarshal(response, &result); err != nil {
			t.Fatalf("decode dispatch %d: %v", attempt, err)
		}
		value := result.Value.(map[string]any)
		if value["text"] != "answer:42" || value["runs"] != wantRuns {
			t.Fatalf("dispatch %d value = %#v", attempt, value)
		}
		if len(result.Commands) != 1 || result.Commands[0].Name != "render.text" {
			t.Fatalf("dispatch %d commands = %#v", attempt, result.Commands)
		}
		if len(result.Events) != 1 || result.Events[0].Name != "composition.completed" {
			t.Fatalf("dispatch %d events = %#v", attempt, result.Events)
		}
	}
}
