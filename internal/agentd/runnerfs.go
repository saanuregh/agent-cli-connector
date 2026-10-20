package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/agentrun"
)

const (
	// runnerFSTimeout bounds one stat, listing, or range read on the far
	// side; runnerStreamTimeout bounds streaming a whole file.
	runnerFSTimeout     = 30 * time.Second
	runnerStreamTimeout = 5 * time.Minute
	// runnerWaitDelay is how long a finished or cancelled wrapper may hold
	// its output open, such as an ssh it started, before the pipes are
	// closed on it.
	runnerWaitDelay = 2 * time.Second
)

// runnerFS reads files on the far side through agentd-run --fs.
type runnerFS struct {
	w wrapperSpawner
}

var _ agent.FileSystem = runnerFS{}

// command returns agentd-run --fs, sending req on its standard input.
func (r runnerFS) command(ctx context.Context, req agentrun.FSRequest) (*exec.Cmd, *bytes.Buffer, error) {
	line, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	c := r.w.runnerCommand("--fs")
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.WaitDelay = runnerWaitDelay
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	return cmd, stderr, nil
}

// runErr maps agentd-run's exit to an error for req.
func runErr(req agentrun.FSRequest, err error, stderr *bytes.Buffer) error {
	if err == nil {
		return nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == agentrun.ExitNotExist {
		return &fs.PathError{Op: req.Op, Path: req.Path, Err: fs.ErrNotExist}
	}
	return &fs.PathError{Op: req.Op, Path: req.Path, Err: fmt.Errorf("agentd-run: %w: %s", err, strings.TrimSpace(stderr.String()))}
}

// do runs one bounded operation and returns its standard output.
func (r runnerFS) do(req agentrun.FSRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runnerFSTimeout)
	defer cancel()
	cmd, stderr, err := r.command(ctx, req)
	if err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := runErr(req, cmd.Run(), stderr); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Open streams the file, so a reader that stops early also stops the
// transfer. The returned file also reads ranges with ReadAt.
func (r runnerFS) Open(path string) (io.ReadCloser, error) {
	req := agentrun.FSRequest{Op: agentrun.OpRead, Path: path}
	ctx, cancel := context.WithTimeout(context.Background(), runnerStreamTimeout)
	cmd, stderr, err := r.command(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, &fs.PathError{Op: req.Op, Path: path, Err: err}
	}
	f := &remoteFile{fs: r, path: path, cmd: cmd, cancel: cancel, stderr: stderr, req: req, out: bufio.NewReader(stdout)}
	if ok, _ := f.out.ReadString('\n'); ok != agentrun.ReadOK {
		err := f.wait()
		if err == nil {
			err = &fs.PathError{Op: req.Op, Path: path, Err: errors.New("agentd-run: no read header")}
		}
		return nil, err
	}
	return f, nil
}

// remoteFile is a file streamed from the far side.
type remoteFile struct {
	fs     runnerFS
	path   string
	req    agentrun.FSRequest
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *bytes.Buffer
	out    *bufio.Reader

	once    sync.Once
	waitErr error
}

// wait ends the transfer once and returns how agentd-run exited.
func (f *remoteFile) wait() error {
	f.once.Do(func() {
		err := f.cmd.Wait()
		f.cancel()
		f.waitErr = runErr(f.req, err, f.stderr)
	})
	return f.waitErr
}

func (f *remoteFile) Read(p []byte) (int, error) {
	n, err := f.out.Read(p)
	if errors.Is(err, io.EOF) {
		if werr := f.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// ReadAt reads a range with its own request, so reading the end of a
// large file transfers only that part.
func (f *remoteFile) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	data, err := f.fs.do(agentrun.FSRequest{Op: agentrun.OpRead, Path: f.path, Offset: off, Length: int64(len(p))})
	if err != nil {
		return 0, err
	}
	data, ok := bytes.CutPrefix(data, []byte(agentrun.ReadOK))
	if !ok {
		return 0, &fs.PathError{Op: agentrun.OpRead, Path: f.path, Err: errors.New("agentd-run: no read header")}
	}
	n := copy(p, data)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Close stops a transfer still running.
func (f *remoteFile) Close() error {
	f.cancel()
	_ = f.wait()
	return nil
}

func (r runnerFS) Stat(path string) (fs.FileInfo, error)  { return r.stat(agentrun.OpStat, path) }
func (r runnerFS) Lstat(path string) (fs.FileInfo, error) { return r.stat(agentrun.OpLstat, path) }

func (r runnerFS) stat(op, path string) (fs.FileInfo, error) {
	data, err := r.do(agentrun.FSRequest{Op: op, Path: path})
	if err != nil {
		return nil, err
	}
	var info agentrun.FileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, &fs.PathError{Op: op, Path: path, Err: err}
	}
	return info.Info(), nil
}

func (r runnerFS) ReadDir(path string) ([]fs.DirEntry, error) {
	data, err := r.do(agentrun.FSRequest{Op: agentrun.OpList, Path: path})
	if err != nil {
		return nil, err
	}
	var infos []agentrun.FileInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		return nil, &fs.PathError{Op: agentrun.OpList, Path: path, Err: err}
	}
	out := make([]fs.DirEntry, len(infos))
	for i, info := range infos {
		out[i] = fs.FileInfoToDirEntry(info.Info())
	}
	return out, nil
}
