package opencode

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Server is a running OpenCode server: a private one this package started,
// or one it attached to through Options.ServerURL.
type Server struct {
	client  *Client
	version string
	proc    *providerkit.Process
	grace   time.Duration
	once    sync.Once
	readEnd chan struct{}
	// stopped reports that Close stopped the server before it exited.
	stopped atomic.Bool
}

// Client returns the API client of the server.
func (s *Server) Client() *Client { return s.client }

// Version returns the server's OpenCode version.
func (s *Server) Version() string { return s.version }

// Private reports whether this package started the server.
func (s *Server) Private() bool { return s.proc != nil }

// Done is closed after a private server exited. It is nil for an
// attached server.
func (s *Server) Done() <-chan struct{} {
	if s.proc == nil {
		return nil
	}
	return s.proc.Exited()
}

// Err returns why a private server exited on its own, or nil while it
// runs, after Close, and for an attached server.
func (s *Server) Err() error {
	if s.proc == nil || s.stopped.Load() {
		return nil
	}
	return s.proc.ExitError()
}

func (s *Server) exitError() error {
	if s.proc == nil {
		return nil
	}
	return s.proc.ExitError()
}

// Close stops a private server: it closes the server's input, which makes
// "serve --stdio" exit, and waits for the exit. It does nothing for an
// attached server.
func (s *Server) Close() error {
	s.once.Do(func() {
		if s.proc != nil {
			select {
			case <-s.proc.Exited():
			default:
				s.stopped.Store(true)
			}
			s.proc.Stop(s.grace)
			<-s.readEnd
		}
	})
	return nil
}

// serverSpec describes a private server to start.
type serverSpec struct {
	dir    string
	env    map[string]string
	unset  []string
	config map[string]any
	logger *slog.Logger
	tap    func(agent.WireDirection, []byte)
	// helper marks a server for provider calls, which loads no project
	// config; see helperEnv.
	helper bool
}

// helperEnv turns off project config for helper servers. OpenCode 2.0.32
// otherwise loads opencode.json, .opencode/opencode.json, and
// .opencode/plugin(s)/*.js from the working directory and every parent,
// so a folder another user can write above the helper's working directory
// would run their code. Both names are honored by 2.0.32 (verified live);
// the global config in the user's config directory still loads, so user
// providers and credentials work.
var helperEnv = map[string]string{
	"OPENCODE_DISABLE_PROJECT_CONFIG": "1",
	"OPENCODE_CONFIG_PROJECT_DISABLE": "1",
}

// Connect returns a server for API calls: the server Options.ServerURL
// names, or else a new private server started in dir (empty means the
// current directory). That server is like a session's: it loads the
// project config and plugins of dir and its parents, unlike the helper
// servers of provider calls, so dir must not lie below a folder other
// users can write. Close the server when done; closing an attached server
// does nothing.
func (p *Provider) Connect(ctx context.Context, dir string) (*Server, error) {
	if p.opts.ServerURL != "" {
		return p.attach(ctx, nil)
	}
	return p.startServer(ctx, serverSpec{dir: dir})
}

func (p *Provider) attach(ctx context.Context, tap func(agent.WireDirection, []byte)) (*Server, error) {
	c, err := newClient(p.opts.ServerURL, p.opts.Username, p.opts.Password, p.opts.HTTPClient, tap)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.startupTimeout())
	defer cancel()
	version, err := verifyServer(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Server{client: c, version: version}, nil
}

// verifyServer reads /api/info and requires OpenCode 2. The health routes
// answer with the web UI's HTML on every version, so only this route proves
// the server is ready.
func verifyServer(ctx context.Context, c *Client) (string, error) {
	var info struct {
		Version string `json:"version"`
	}
	if err := c.Call(ctx, "GET", "/api/info", nil, &info); err != nil {
		if httpStatus(err) == 401 {
			return "", fmt.Errorf("opencode: the server rejected the password: %w", err)
		}
		return "", fmt.Errorf("opencode: %s is not a reachable OpenCode 2 server: %w", c.redactedURL(), err)
	}
	if major(info.Version) < 2 {
		return "", fmt.Errorf("opencode: server version %q is not supported; OpenCode 2 is required", info.Version)
	}
	return info.Version, nil
}

// major returns the major version of v, or 0.
func major(v string) int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	head, _, _ := strings.Cut(v, ".")
	n, _ := strconv.Atoi(head)
	return n
}

var listeningPattern = regexp.MustCompile(`listening on\s+(https?://\S+)`)

// serverURLOf returns the URL a starting server printed on line: a JSON
// {"url": ...} object for "serve --stdio", or "server listening on URL".
func serverURLOf(line []byte) string {
	var msg struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(line, &msg) == nil && msg.URL != "" {
		return msg.URL
	}
	if m := listeningPattern.FindSubmatch(line); m != nil {
		return string(m[1])
	}
	return ""
}

func newPassword() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// startServer starts a private server with "serve --stdio".
func (p *Provider) startServer(ctx context.Context, spec serverSpec) (*Server, error) {
	logger := spec.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	password := newPassword()
	env := map[string]string{}
	maps.Copy(env, p.opts.Env)
	maps.Copy(env, spec.env)
	if len(spec.config) > 0 {
		content, err := mergeConfig(cmp.Or(env["OPENCODE_CONFIG_CONTENT"], os.Getenv("OPENCODE_CONFIG_CONTENT")), spec.config)
		if err != nil {
			return nil, err
		}
		env["OPENCODE_CONFIG_CONTENT"] = content
	}
	// OPENCODE_PASSWORD wins over the 1.x OPENCODE_SERVER_PASSWORD; the
	// inherited 1.x variable is dropped so only this password is in play.
	env["OPENCODE_PASSWORD"] = password
	if spec.helper {
		maps.Copy(env, helperEnv)
	}
	unset := append(append([]string(nil), spec.unset...), "OPENCODE_SERVER_PASSWORD")
	args := []string{"serve", "--stdio", "--hostname", cmp.Or(p.opts.Hostname, "127.0.0.1"), "--port", strconv.Itoa(p.opts.Port)}
	args = append(args, p.opts.ExtraArgs...)
	proc, err := providerkit.StartProcess(ctx, providerkit.ProcessOptions{
		Provider: Name,
		Spawner:  p.opts.Spawner,
		Command:  agent.Command{Path: p.path(), Args: args, Env: providerkit.EnvList(env), UnsetEnv: unset, Dir: spec.dir},
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	srv := &Server{proc: proc, grace: p.shutdownGrace(), readEnd: make(chan struct{})}
	urls := make(chan string, 1)
	go func() {
		defer close(srv.readEnd)
		_ = proc.ReadLines(func(line []byte) {
			if u := serverURLOf(line); u != "" {
				select {
				case urls <- u:
				default:
				}
				return
			}
			logger.Debug("opencode server output", "line", string(line))
		})
	}()
	fail := func(err error) (*Server, error) {
		_ = srv.Close()
		return nil, err
	}
	timer := time.NewTimer(p.startupTimeout())
	defer timer.Stop()
	var raw string
	select {
	case raw = <-urls:
	case <-proc.Exited():
		return fail(fmt.Errorf("opencode: server exited during startup: %w", proc.ExitError()))
	case <-timer.C:
		return fail(fmt.Errorf("opencode: server did not report its URL within %s", p.startupTimeout()))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	if p.opts.URLHost != "" {
		mapped, err := replaceHost(raw, p.opts.URLHost)
		if err != nil {
			return fail(fmt.Errorf("opencode: URLHost: %w", err))
		}
		raw = mapped
	}
	if p.opts.MapURL != nil {
		mapped, err := p.opts.MapURL(raw)
		if err != nil {
			return fail(fmt.Errorf("opencode: map server URL: %w", err))
		}
		raw = mapped
	}
	c, err := newClient(raw, "opencode", password, p.opts.HTTPClient, spec.tap)
	if err != nil {
		return fail(err)
	}
	srv.client = c
	vctx, cancel := context.WithTimeout(ctx, p.startupTimeout())
	defer cancel()
	version, err := verifyServer(vctx, c)
	if err != nil {
		if exitErr := proc.ExitError(); exitErr != nil {
			err = exitErr
		}
		return fail(err)
	}
	srv.version = version
	return srv, nil
}

// mergeConfig merges extra into the JSON object base, recursively for
// nested objects.
func mergeConfig(base string, extra map[string]any) (string, error) {
	merged := map[string]any{}
	if strings.TrimSpace(base) != "" {
		if err := json.Unmarshal([]byte(base), &merged); err != nil {
			return "", fmt.Errorf("opencode: OPENCODE_CONFIG_CONTENT is not a JSON object: %w", err)
		}
	}
	deepMerge(merged, extra)
	data, err := json.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("opencode: encode OPENCODE_CONFIG_CONTENT: %w", err)
	}
	return string(data), nil
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if cur, ok := dst[k].(map[string]any); ok {
				deepMerge(cur, sub)
				continue
			}
			cp := map[string]any{}
			deepMerge(cp, sub)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// serverLease is a session's use of a server; release gives it back.
type serverLease struct {
	*Server
	shared  bool
	release func()
}

// sharedServer is the private server Options.SharedServer shares.
type sharedServer struct {
	mu   sync.Mutex
	srv  *Server
	refs int
}

func (p *Provider) acquireShared(ctx context.Context, logger *slog.Logger) (*serverLease, error) {
	sh := &p.shared
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.srv != nil && sh.srv.exitError() != nil {
		_ = sh.srv.Close()
		sh.srv, sh.refs = nil, 0
	}
	if sh.srv == nil {
		srv, err := p.startServer(ctx, serverSpec{logger: logger})
		if err != nil {
			return nil, err
		}
		sh.srv = srv
	}
	srv, release := sh.holdLocked()
	return &serverLease{Server: srv, shared: true, release: release}, nil
}

// holdLocked takes a reference on the running shared server; sh.mu must
// be held. The release func drops it once, stopping the server when it
// was the last.
func (sh *sharedServer) holdLocked() (*Server, func()) {
	srv := sh.srv
	sh.refs++
	var once sync.Once
	return srv, func() {
		once.Do(func() {
			sh.mu.Lock()
			defer sh.mu.Unlock()
			if sh.srv != srv {
				return
			}
			if sh.refs--; sh.refs <= 0 {
				_ = srv.Close()
				sh.srv, sh.refs = nil, 0
			}
		})
	}
}

// leaseLiveShared returns the shared server while it runs, holding a
// reference until release, so a session closing meanwhile cannot stop it
// under the caller. It never starts a server; it returns nil when none
// runs.
func (p *Provider) leaseLiveShared() (srv *Server, release func()) {
	sh := &p.shared
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.srv == nil || sh.srv.exitError() != nil {
		return nil, nil
	}
	return sh.holdLocked()
}

// withClient runs fn with a client for provider-level calls: the attached
// server, the running shared server, or a private server started for the
// call and stopped after it.
func (p *Provider) withClient(ctx context.Context, fn func(*Client) error) error {
	if p.opts.ServerURL != "" {
		srv, err := p.attach(ctx, nil)
		if err != nil {
			return err
		}
		return fn(srv.client)
	}
	if srv, release := p.leaseLiveShared(); srv != nil {
		defer release()
		return fn(srv.client)
	}
	// One stable directory, since OpenCode records a project for every
	// directory a server serves; see helperDir.
	dir, err := p.helperDir()
	if err != nil {
		return err
	}
	srv, err := p.startServer(ctx, serverSpec{dir: dir, helper: true})
	if err != nil {
		return err
	}
	defer srv.Close()
	return fn(srv.client)
}
