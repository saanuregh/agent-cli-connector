package pi_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// TestBridgeListedInCommandFiles checks that the bridge the library writes
// is handed to the spawner, which a remote one must place at the same
// path, and that a BridgePath already in place is not.
func TestBridgeListedInCommandFiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LocalAppData", t.TempDir())
	for _, tc := range []struct {
		bridgePath string
		wantFile   bool
	}{{"", true}, {"/remote/bridge.ts", false}} {
		spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
			if startup(h) {
				h.WaitClosed()
			}
		})
		sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: tc.bridgePath}).StartSession(context.Background(), agent.Config{})
		if err != nil {
			t.Fatal(err)
		}
		cmd := (<-started).Cmd
		_ = sess.Close()
		for range sess.Events() {
		}
		if !tc.wantFile {
			if len(cmd.Files) != 0 || !slices.Contains(cmd.Args, tc.bridgePath) {
				t.Errorf("BridgePath %q: files %v, args %v", tc.bridgePath, cmd.Files, cmd.Args)
			}
			continue
		}
		if len(cmd.Files) != 1 || !bytes.Equal(cmd.Files[0].Data, pi.BridgeSource()) || !slices.Contains(cmd.Args, cmd.Files[0].Path) {
			t.Errorf("local bridge: files %+v, args %v", cmd.Files, cmd.Args)
		}
	}
}
