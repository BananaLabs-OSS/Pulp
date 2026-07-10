// Hand-written domain handler (USER code — the witcell generator emits the ABI
// glue in witcell_gen.go; the developer writes only this). Same resolve logic
// as the proven witcell prototype.
package main

import "strings"

func (e Engine) debugName() string {
	switch e {
	case Vanilla:
		return "Vanilla"
	case Paper:
		return "Paper"
	case Fabric:
		return "Fabric"
	case Bedrock:
		return "Bedrock"
	}
	return "Unknown"
}

type resolveError struct{ msg string }

func (e *resolveError) Error() string { return e.msg }

func resolve(req ResolveRequest) (ResolveResponse, error) {
	if req.Engine == Vanilla || req.Engine == Bedrock {
		if req.ModsJson != nil && *req.ModsJson != "" {
			return ResolveResponse{}, &resolveError{
				"unsupported-mods: engine " + req.Engine.debugName() + " does not support mods_json",
			}
		}
	}

	version := "1.21.4"
	if req.Version != nil {
		version = *req.Version
	}

	ram := 0.0
	switch req.Engine {
	case Vanilla:
		ram = 2048.0
	case Bedrock:
		ram = 1024.0
	case Paper:
		ram = 3072.0
	case Fabric:
		ram = 4096.0
	}

	var mods []string
	if req.Engine == Fabric {
		mods = []string{"fabric-api-0.115.0", "lithium-0.14.7"}
	}

	url := strings.ToLower("https://cdn.example.net/" + req.Engine.debugName() + "/" + version + "/server.jar")

	return ResolveResponse{
		ServerJarUrl: url,
		Plugins:      []string{"ViaVersion-5.1.1", "Geyser-2.4.2", "floodgate-2.2.4"},
		Mods:         mods,
		Datapacks:    []string{"vanilla-tweaks-1.21"},
		CpuWeight:    1.5,
		RamWeightMb:  ram,
		EnvOverrides: []EnvVar{
			{"JAVA_TOOL_OPTIONS", "-XX:+UseG1GC"},
			{"EULA", "true"},
			{"MC_VERSION", version},
		},
	}, nil
}
