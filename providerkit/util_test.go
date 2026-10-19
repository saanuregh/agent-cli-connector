package providerkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestCheckInput(t *testing.T) {
	for _, tc := range []struct {
		in   agent.Input
		want string
	}{
		{agent.Text("x"), ""},
		{agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: agent.DeliverSteer}, ""},
		{agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: "Steer"}, `unknown Input.Delivery "Steer"`},
		{agent.Input{Parts: []agent.Part{agent.TextPart("x"), {Type: "audio"}}}, `Input.Parts[1] has unknown type "audio"`},
	} {
		err := CheckInput("p", tc.in)
		if tc.want == "" {
			if err != nil {
				t.Errorf("CheckInput(%+v) = %v", tc.in, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CheckInput(%+v) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

func TestCheckUpdatedInput(t *testing.T) {
	in := []byte(`{"command":"ls"}`)
	if err := CheckUpdatedInput("p", agent.PermissionResponse{Decision: agent.DecisionAllow, UpdatedInput: in}); err == nil {
		t.Error("allow with UpdatedInput accepted")
	}
	for _, resp := range []agent.PermissionResponse{
		{Decision: agent.DecisionAllow},
		{Decision: agent.DecisionDeny, UpdatedInput: in},
		{Decision: agent.DecisionAllow, UpdatedInput: in, ProviderResponse: "native"},
	} {
		if err := CheckUpdatedInput("p", resp); err != nil {
			t.Errorf("CheckUpdatedInput(%+v) = %v", resp, err)
		}
	}
}

func TestCheckPermissionMode(t *testing.T) {
	supported := []agent.PermissionMode{agent.PermissionDefault, agent.PermissionPlan}
	for _, mode := range []agent.PermissionMode{"", agent.PermissionPlan} {
		if err := CheckPermissionMode("p", mode, supported); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	err := CheckPermissionMode("p", agent.PermissionBypass, supported)
	if !errors.Is(err, agent.ErrUnsupported) || err.Error() != Unsupported("p", "permission mode bypassPermissions").Error() {
		t.Errorf("unsupported mode = %v", err)
	}
}

func TestPageSessionFiles(t *testing.T) {
	now := time.Now()
	files := []SessionFile{
		{"old", now.Add(-3 * time.Hour)},
		{"skip", now.Add(-time.Minute)},
		{"new", now},
		{"mid", now.Add(-time.Hour)},
	}
	summarize := func(path string) (agent.SessionSummary, bool) {
		sum := agent.SessionSummary{ID: path}
		if path == "mid" {
			sum.UpdatedAt = now.Add(time.Hour)
		}
		return sum, path != "skip"
	}
	ids := func(sums []agent.SessionSummary) string {
		var out []string
		for _, s := range sums {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	all, err := PageSessionFiles(context.Background(), files, 0, 0, summarize)
	if err != nil || ids(all) != "new,mid,old" || !all[0].UpdatedAt.Equal(now) || !all[1].UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Errorf("all = %+v, %v", all, err)
	}
	if page, err := PageSessionFiles(context.Background(), files, 1, 1, summarize); err != nil || ids(page) != "mid" {
		t.Errorf("page = %s, %v; want mid (skipped files do not count toward offset)", ids(page), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := PageSessionFiles(ctx, files, 0, 0, summarize); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Errorf("cancelled = %+v, %v", got, err)
	}
}

func TestCacheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	root := filepath.Join(cache, "agent-cli-connector")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := CacheDir("opencode")
	if err != nil || dir != filepath.Join(root, "opencode") {
		t.Fatalf("CacheDir = %q, %v", dir, err)
	}
	for _, d := range []string{root, dir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if unixModes && info.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v; want 0700", d, info.Mode().Perm())
		}
	}
	if got, err := CacheDir(""); err != nil || got != root {
		t.Errorf("CacheDir(\"\") = %q, %v", got, err)
	}
	if err := os.Symlink(home, filepath.Join(root, "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symlink needs a privilege on Windows: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := CacheDir("link"); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Errorf("symlinked cache directory = %v", err)
	}
}
