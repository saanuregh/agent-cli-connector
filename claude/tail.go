package claude

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Polling limits for shell output tails.
const (
	tailPollInterval = 250 * time.Millisecond
	tailLocateTries  = 8
	tailMaxReadBytes = 256 << 10
	tmpDirEnv        = "CLAUDE_CODE_TMPDIR"
)

// outputTail streams a foreground Bash call's output file as
// ToolOutputDelta events until stopped.
type outputTail struct {
	stop chan struct{}
	done chan struct{}
}

// startTail follows the CLI's private output file of a foreground shell
// task: <CLAUDE_CODE_TMPDIR or the temp dir>/claude-<uid>/<project>/<session>/tasks/<task>.output.
// When that file cannot be found, or its root directory does not exist
// here (such as with a remote spawner), it polls get_task_output instead.
// It does nothing unless SessionOptions.TailShellOutput is set.
func (t *translator) startTail(toolUseID, taskID string) {
	if !t.s.opts.TailShellOutput {
		return
	}
	if call, ok := t.tools[toolUseID]; !ok || call.Tool == nil || call.Tool.Kind != agent.ToolShell {
		return
	}
	root := ""
	if uid := os.Getuid(); uid >= 0 {
		root = filepath.Join(t.s.tempDir(), "claude-"+strconv.Itoa(uid))
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			root = ""
		}
	}
	tail := &outputTail{stop: make(chan struct{}), done: make(chan struct{})}
	t.tailMu.Lock()
	defer t.tailMu.Unlock()
	if t.tails[toolUseID] != nil {
		return
	}
	t.tails[toolUseID] = tail
	go tail.run(t.s, root, t.s.ID(), taskID, toolUseID)
}

// stopTail stops the tail of a tool call and waits for it, so no delta
// follows the call's completion.
func (t *translator) stopTail(toolUseID string) {
	t.tailMu.Lock()
	tail := t.tails[toolUseID]
	delete(t.tails, toolUseID)
	t.tailMu.Unlock()
	if tail == nil {
		return
	}
	close(tail.stop)
	<-tail.done
}

// tempDir is the CLI's temp root: CLAUDE_CODE_TMPDIR when set, else the
// system temp directory.
func (s *Session) tempDir() string {
	for _, env := range []map[string]string{s.cfg.Env, s.provider.opts.Env} {
		if d := env[tmpDirEnv]; d != "" {
			return d
		}
	}
	if d := os.Getenv(tmpDirEnv); d != "" {
		return d
	}
	return os.TempDir()
}

func (tail *outputTail) run(s *Session, root, sessionID, taskID, itemID string) {
	defer close(tail.done)
	tick := time.NewTicker(tailPollInterval)
	defer tick.Stop()
	wait := func() bool {
		select {
		case <-tail.stop:
			return false
		case <-s.rt.Done():
			return false
		case <-tick.C:
			return true
		}
	}
	var path string
	for try := 0; path == "" && root != ""; try++ {
		matches, _ := filepath.Glob(filepath.Join(root, "*", sessionID, "tasks", taskID+".output"))
		if len(matches) > 0 {
			path = matches[0]
			break
		}
		if try+1 >= tailLocateTries {
			break
		}
		if !wait() {
			return
		}
	}
	if path == "" {
		tail.poll(s, taskID, itemID, wait)
		return
	}
	var offset int64
	var carry []byte
	for {
		if info, err := os.Stat(path); err == nil {
			size := info.Size()
			if size < offset {
				offset, carry = 0, nil
			}
			if size > offset {
				start, prefix := offset, ""
				if size-offset > tailMaxReadBytes {
					start, prefix, carry = size-tailMaxReadBytes, "\n", nil
				}
				if data, err := readRange(path, start, size-start); err == nil {
					offset = start + int64(len(data))
					data = append(carry, data...)
					cut := completeUTF8(data)
					carry = append([]byte(nil), data[cut:]...)
					if text := prefix + string(data[:cut]); text != "" {
						s.rt.Emit(&agent.ToolOutputDelta{ItemID: itemID, Delta: text})
					}
				}
			}
		}
		if !wait() {
			return
		}
	}
}

// poll streams a task's output through get_task_output, which returns the
// last 8 KiB and the total size. It stops when the CLI refuses the task.
// The request is cancelled when the tail stops, because stopTail runs on
// the reader goroutine, which must stay free to deliver the answer.
func (tail *outputTail) poll(s *Session, taskID, itemID string, wait func() bool) {
	ctx, cancel := context.WithCancel(s.rt.Context())
	defer cancel()
	go func() {
		select {
		case <-tail.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	var seen int64
	for {
		var r struct {
			Output     string `json:"output"`
			TotalBytes int64  `json:"total_bytes"`
		}
		if err := s.controlInto(ctx, "get_task_output", map[string]any{"task_id": taskID}, &r); err != nil {
			return
		}
		if r.TotalBytes < seen {
			seen = 0
		}
		if fresh := r.TotalBytes - seen; fresh > 0 {
			text, out := "", []byte(r.Output)
			if fresh <= int64(len(out)) {
				start := len(out) - int(fresh)
				for start < len(out) && !utf8.RuneStart(out[start]) {
					start++
				}
				text = string(out[start:])
			} else if seen > 0 {
				text = "\n" + r.Output
			} else {
				text = r.Output
			}
			seen = r.TotalBytes
			if text != "" {
				s.rt.Emit(&agent.ToolOutputDelta{ItemID: itemID, Delta: text})
			}
		}
		if !wait() {
			return
		}
	}
}

func readRange(path string, start, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:m], nil
}

// completeUTF8 returns the length of the longest prefix of b that does not
// end inside a multi-byte character.
func completeUTF8(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}
