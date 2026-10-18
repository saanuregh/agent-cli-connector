// Package livetest keeps live tests from leaving sessions in the user's
// harness stores.
package livetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Store deletes the sessions a test ran when the test ends.
type Store struct {
	t        testing.TB
	provider agent.Provider
	cwd      string
	mu       sync.Mutex
	sessions []agent.Session
	ids      []string
}

// New returns a Store for sessions that provider runs in cwd, which must
// be a directory the test created under the system temporary directory.
// When the test ends, the Store deletes every tracked session, and for
// Claude also cwd's project folder and the empty per-session folders the
// CLI creates even for ephemeral sessions.
func New(t testing.TB, provider agent.Provider, cwd string) *Store {
	t.Helper()
	if !underTemp(cwd) {
		t.Fatalf("livetest: %s is not under %s", cwd, os.TempDir())
	}
	s := &Store{t: t, provider: provider, cwd: cwd}
	t.Cleanup(s.cleanup)
	return s
}

// Cwd returns the directory the Store was created for.
func (s *Store) Cwd() string { return s.cwd }

// Track records a session for deletion. Call it right after the session
// starts; its ID is read again at cleanup, because a rewind can change it.
func (s *Store) Track(sess agent.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, sess)
	s.ids = append(s.ids, sess.ID())
}

func (s *Store) cleanup() {
	s.mu.Lock()
	ids := append([]string(nil), s.ids...)
	for _, sess := range s.sessions {
		_ = sess.Close()
		ids = append(ids, sess.ID())
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	seen := map[string]bool{}
	isClaude := s.provider.Name() == "claude"
	var projectDirs []string
	if isClaude {
		projectDirs = claudeProjectDirs(s.cwd)
	}
	for _, dir := range projectDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok {
				ids = append(ids, id)
			}
		}
	}
	del, _ := s.provider.(agent.SessionDeleter)
	// Newest first: Codex refuses to delete a thread a fork still
	// references.
	slices.Reverse(ids)
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if del != nil {
			if err := del.DeleteSession(ctx, id, s.cwd); err != nil && !errors.Is(err, agent.ErrSessionNotFound) {
				s.t.Logf("livetest: delete session %s: %v", id, err)
			}
		}
		if isClaude {
			// Remove succeeds only on an empty folder.
			_ = os.Remove(filepath.Join(claudeConfigDir(), "session-env", id))
		}
	}
	for _, dir := range projectDirs {
		if err := os.RemoveAll(dir); err != nil {
			s.t.Logf("livetest: %v", err)
		}
	}
}

// PiEnv returns the environment that keeps Pi, and the subagents it
// starts, storing sessions in a directory the test owns.
func PiEnv(t testing.TB) map[string]string {
	return map[string]string{"PI_CODING_AGENT_SESSION_DIR": t.TempDir()}
}

func underTemp(dir string) bool {
	rel, err := filepath.Rel(os.TempDir(), dir)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// claudeProjectDirs returns cwd's project folders for the path as given
// and symlink-resolved. Test directories are short, so the long-path hash
// suffix the CLI adds never applies.
func claudeProjectDirs(cwd string) []string {
	paths := []string{cwd}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		paths = append(paths, real)
	}
	var dirs []string
	for _, p := range paths {
		b := []byte(p)
		for i, c := range b {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				b[i] = '-'
			}
		}
		if len(b) > 200 {
			continue
		}
		dirs = append(dirs, filepath.Join(claudeConfigDir(), "projects", string(b)))
	}
	return dirs
}

// Recorder collects a session's events.
type Recorder struct {
	mu     sync.Mutex
	events []agent.Event
}

// Drain records every event until sess closes its event stream.
func (r *Recorder) Drain(sess agent.Session) {
	for ev := range sess.Events() {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	}
}

// All returns a copy of the recorded events.
func (r *Recorder) All() []agent.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Event(nil), r.events...)
}
