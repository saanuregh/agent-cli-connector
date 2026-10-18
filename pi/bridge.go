package pi

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/saanuregh/agent-cli-connector/providerkit"
)

//go:embed bridge.ts
var bridgeSource []byte

// BridgeSource returns the bridge extension's TypeScript source, for hosts
// that place it themselves (see Options.BridgePath).
func BridgeSource() []byte { return append([]byte(nil), bridgeSource...) }

var (
	bridgeOnce sync.Once
	bridgeFile string
	bridgeErr  error
)

// staleBridge is how long an unused bridge version is kept. Pi reads the
// file again whenever it replaces a session, so a version that a running
// Pi may still use must stay.
const staleBridge = 30 * 24 * time.Hour

// pruneBridges removes bridge versions other than keep that no process
// wrote or used for staleBridge.
func pruneBridges(dir, keep string) {
	paths, _ := filepath.Glob(filepath.Join(dir, "pi-bridge-*.ts"))
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && p != keep && time.Since(info.ModTime()) > staleBridge {
			_ = os.Remove(p)
		}
	}
}

// localBridgePath writes the bridge to a content-addressed file in the
// user cache directory once per process.
func localBridgePath() (string, error) {
	bridgeOnce.Do(func() {
		var dir string
		if _, err := os.UserCacheDir(); err == nil {
			if dir, err = providerkit.CacheDir(""); err != nil {
				bridgeErr = fmt.Errorf("pi: bridge directory: %w", err)
				return
			}
		} else if dir, err = os.MkdirTemp("", "agent-cli-connector-"); err != nil {
			// A private directory: a shared one could let another user
			// replace the extension Pi loads.
			bridgeErr = fmt.Errorf("pi: create bridge directory: %w", err)
			return
		}
		sum := sha256.Sum256(bridgeSource)
		path := filepath.Join(dir, "pi-bridge-"+hex.EncodeToString(sum[:8])+".ts")
		defer pruneBridges(dir, path)
		if existing, err := os.ReadFile(path); err == nil && string(existing) == string(bridgeSource) {
			bridgeFile = path
			return
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			bridgeErr = fmt.Errorf("pi: read bridge: %w", err)
			return
		}
		tmp, err := os.CreateTemp(dir, "pi-bridge-*.ts")
		if err != nil {
			bridgeErr = fmt.Errorf("pi: write bridge: %w", err)
			return
		}
		_, werr := tmp.Write(bridgeSource)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmp.Name())
			bridgeErr = fmt.Errorf("pi: write bridge: %w", errors.Join(werr, cerr))
			return
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			bridgeErr = fmt.Errorf("pi: write bridge: %w", err)
			return
		}
		bridgeFile = path
	})
	if bridgeFile != "" {
		// Marks the version in use on every start, for long-running hosts.
		now := time.Now()
		_ = os.Chtimes(bridgeFile, now, now)
	}
	return bridgeFile, bridgeErr
}
