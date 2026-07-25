package run

import (
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// loadManifestInputs selects the first-class app path or the legacy repeated
// cell-manifest path. Exactly one input style is required.
func loadManifestInputs(appPath string, manifestPaths []string) (*manifest.Set, *manifest.Application, error) {
	if appPath != "" && len(manifestPaths) > 0 {
		return nil, nil, fmt.Errorf("-app cannot be combined with -manifest")
	}
	if appPath == "" && len(manifestPaths) == 0 {
		return nil, nil, fmt.Errorf("one of -app or -manifest is required")
	}
	if appPath != "" {
		app, err := manifest.LoadApp(appPath)
		if err != nil {
			return nil, nil, err
		}
		return app.Cells, app, nil
	}
	set, err := manifest.LoadAll(manifestPaths)
	if err != nil {
		return nil, nil, err
	}
	return set, nil, nil
}
