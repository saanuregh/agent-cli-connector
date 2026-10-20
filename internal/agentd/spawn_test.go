package agentd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
)

func TestWrapperSpawnerCommand(t *testing.T) {
	sp, err := (&SpawnerConfig{Wrapper: []string{"ssh", "box", "run"}}).spawner()
	if err != nil {
		t.Fatal(err)
	}
	cmd := sp.(wrapperSpawner).wrap(agent.Command{Path: "claude", Args: []string{"-p", "x"}, Env: []string{"A=1", "B=x=y"}, UnsetEnv: []string{"C"}, Dir: "/repo",
		Files: []agent.File{{Path: "/cache/bridge.ts", Data: []byte("export {}")}}})
	if !slices.Contains(cmd.Env, `AGENTD_FILES=[{"Path":"/cache/bridge.ts","Data":"ZXhwb3J0IHt9"}]`) {
		t.Errorf("files env %q", cmd.Env)
	}
	if cmd.Path != "ssh" || !slices.Equal(cmd.Args, []string{"box", "run", "claude", "-p", "x"}) || cmd.Dir != "" {
		t.Errorf("command %+v", cmd)
	}
	var env map[string]string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "AGENTD_ENV="); ok {
			_ = json.Unmarshal([]byte(v), &env)
		}
	}
	if env["A"] != "1" || env["B"] != "x=y" || !slices.Contains(cmd.Env, "AGENTD_DIR=/repo") || !slices.Contains(cmd.Env, `AGENTD_UNSET_ENV=["C"]`) || !slices.Contains(cmd.Env, "A=1") {
		t.Errorf("env %q", cmd.Env)
	}
	if _, err := (&SpawnerConfig{}).spawner(); errorKind(err) != "invalid_params" {
		t.Errorf("empty wrapper: %v", err)
	}
}

func TestWrapperSpawnerRunsHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell wrapper")
	}
	out := filepath.Join(t.TempDir(), "out")
	script := `printf '%s\n' "$@" "$AGENTD_DIR" "$AGENTD_ENV" > "` + out + `"`
	sp, _ := (&SpawnerConfig{Wrapper: []string{"sh", "-c", script, "wrapper"}}).spawner()
	proc, err := sp.Spawn(context.Background(), agent.Command{Path: "codex", Args: []string{"app-server"}, Env: []string{"K=v"}, Dir: "/far/repo"})
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Stdin().Close()
	if err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if want := "codex\napp-server\n/far/repo\n{\"K\":\"v\"}\n"; string(got) != want {
		t.Errorf("wrapper saw %q, want %q", got, want)
	}
}

func TestSpawnerReachesProviderOptions(t *testing.T) {
	sp, _ := (&SpawnerConfig{Wrapper: []string{"w"}}).spawner()
	for _, spec := range Defaults() {
		if _, err := spec.New(nil, ProviderBridge{Spawner: sp}); err != nil {
			t.Errorf("%s: %v", spec.Name, err)
		}
	}
	type options struct{ Spawner agent.Spawner }
	var built options
	newProvider := plainProvider("test", func(o options) *agenttest.Provider {
		built = o
		return agenttest.New(echo)
	}, func(o *options) *agent.Spawner { return &o.Spawner })
	if _, err := newProvider(nil, ProviderBridge{Spawner: sp}); err != nil || built.Spawner == nil {
		t.Errorf("Spawner not set: %v", err)
	}
	if _, err := newProvider(nil, ProviderBridge{}); err != nil || built.Spawner != nil {
		t.Errorf("no Spawner replaced the default: %v", err)
	}
}

func TestInitializeRejectsEmptyWrapper(t *testing.T) {
	c := newClient(t, Defaults()...)
	_, info := errorInfoOf(t, c.call("initialize", map[string]any{"Providers": map[string]any{"codex": map[string]any{"Spawner": map[string]any{"Wrapper": []string{}}}}}, nil))
	if info.Kind != "invalid_params" {
		t.Errorf("empty wrapper: %+v", info)
	}
}
