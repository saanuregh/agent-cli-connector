package opencode

import (
	"fmt"
	"os"

	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// helperDir returns the working directory of the servers that provider
// calls start: Options.HelperDir when set, otherwise
// agent-cli-connector/opencode under the user cache directory. The folders
// the library uses must be plain directories this user owns, and are kept
// at mode 0700. Folders above them are not checked: helper servers run
// with project config turned off (see helperEnv), so configs and plugins
// planted in a parent folder are not loaded.
func (p *Provider) helperDir() (string, error) {
	if dir := p.opts.HelperDir; dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("opencode: create helper directory: %w", err)
		}
		return dir, providerkit.CheckOwnDir(dir, "opencode: helper directory")
	}
	dir, err := providerkit.CacheDir("opencode")
	if err != nil {
		return "", fmt.Errorf("opencode: helper directory (set Options.HelperDir to use another): %w", err)
	}
	return dir, nil
}
