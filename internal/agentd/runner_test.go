package agentd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// buildRunner builds cmd/agentd-run for this machine, which plays the far
// side.
func buildRunner(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agentd-run")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/agentd-run").CombinedOutput(); err != nil {
		t.Fatalf("build agentd-run: %v\n%s", err, out)
	}
	return bin
}

func TestRunnerStartsHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness here is a POSIX shell script")
	}
	if testing.Short() {
		t.Skip("builds a binary")
	}
	sp, _ := (&SpawnerConfig{Wrapper: []string{buildRunner(t)}, Runner: true}).spawner()
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "deep", "bridge.ts")
	t.Setenv("AGENTD_TEST_DROP", "x") // inherited by the runner, which must remove it
	script := `printf '%s|%s|%s|%s\n' "$FOO" "$PWD" "$(cat "$1")" "${AGENTD_TEST_DROP-unset}"; read line; echo "got $line"`
	proc, err := sp.Spawn(context.Background(), agent.Command{
		Path: "sh", Args: []string{"-c", script, "harness", file},
		Dir: dir, Env: []string{"FOO=a b"}, UnsetEnv: []string{"AGENTD_TEST_DROP"},
		Files: []agent.File{{Path: file, Data: []byte("export {}")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := bufio.NewReader(proc.Stdout())
	first, _ := out.ReadString('\n')
	realDir, _ := filepath.EvalSymlinks(dir)
	if want := "a b|" + realDir + "|export {}|unset\n"; first != want && first != "a b|"+dir+"|export {}|unset\n" {
		t.Errorf("harness saw %q, want %q", first, want)
	}
	if _, err := io.WriteString(proc.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	if second, _ := out.ReadString('\n'); second != "got hello\n" {
		t.Errorf("harness read %q after the setup line", second)
	}
	_ = proc.Stdin().Close()
	if err := proc.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestRunnerFileSystem(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	sp, _ := (&SpawnerConfig{Wrapper: []string{buildRunner(t)}, Runner: true}).spawner()
	fsys := agent.FileSystemOf(sp)
	if fsys == agent.LocalFileSystem {
		t.Fatal("a runner spawner has no file system of its own")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := providerkit.ReadFile(fsys, filepath.Join(root, "a.jsonl"))
	if err != nil || string(data) != "line\n" {
		t.Errorf("read = %q, %v", data, err)
	}
	info, err := fsys.Stat(filepath.Join(root, "a.jsonl"))
	if err != nil || info.Size() != 5 || info.IsDir() || info.ModTime().IsZero() {
		t.Errorf("stat = %+v, %v", info, err)
	}
	entries, err := fsys.ReadDir(root)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if err != nil || !slices.Equal(names, []string{"a.jsonl", "sub"}) || !entries[1].IsDir() {
		t.Errorf("list = %v, %v", names, err)
	}
	if _, err := fsys.Open(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	// A large file: ReadAt fetches only the range, and closing a stream
	// early stops it.
	big := bytes.Repeat([]byte("0123456789"), 1<<20) // 10 MiB
	big[len(big)-1] = '\n'
	if err := os.WriteFile(filepath.Join(root, "big.out"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open(filepath.Join(root, "big.out"))
	if err != nil {
		t.Fatal(err)
	}
	at, ok := f.(io.ReaderAt)
	if !ok {
		t.Fatal("a remote file does not support ReadAt")
	}
	last := make([]byte, 1)
	if n, err := at.ReadAt(last, int64(len(big)-1)); n != 1 || err != nil || last[0] != '\n' {
		t.Errorf("ReadAt(last byte) = %d, %v, %q", n, err, last)
	}
	head := make([]byte, 10)
	if _, err := io.ReadFull(f, head); err != nil || string(head) != "0123456789" {
		t.Errorf("streamed head = %q, %v", head, err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("close mid-stream: %v", err)
	}
	if matches, _ := providerkit.Glob(fsys, filepath.Join(root, "*.jsonl")); len(matches) != 1 {
		t.Errorf("glob through the runner = %v", matches)
	}
	if local, _ := (&SpawnerConfig{Wrapper: []string{"ssh"}}).spawner(); agent.FileSystemOf(local) != agent.LocalFileSystem {
		t.Error("a plain wrapper claims a remote file system")
	}
}

// A wrapper that never reads its input cannot hold Spawn past its context.
func TestRunnerSetupWriteHonorsContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	sp, _ := (&SpawnerConfig{Wrapper: []string{"sh", "-c", "sleep 30", "wrapper"}, Runner: true}).spawner()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	big := agent.File{Path: "/x", Data: bytes.Repeat([]byte("x"), 4<<20)} // more than a pipe holds
	start := time.Now()
	if _, err := sp.Spawn(ctx, agent.Command{Path: "true", Files: []agent.File{big}}); err == nil {
		t.Fatal("Spawn succeeded with a wrapper that never reads")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Spawn took %s past its 200ms context", took)
	}
}
