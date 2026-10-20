// Package agentrun is the protocol between agentd and agentd-run, the
// runner that starts harnesses and reads their files on another machine.
// Everything goes over the runner's standard input, never its command
// line, so wrappers such as ssh that re-split arguments through a shell
// pass it unchanged.
//
// agentd-run --exec reads one Setup line, applies it, and replaces itself
// with the harness, which then owns the rest of standard input and output.
//
// agentd-run --fs reads one FSRequest line. A read writes ReadOK and then
// the file's bytes, from Offset and at most Length of them when Length is
// set; stat, lstat, and list write JSON (FileInfo or []FileInfo). A
// missing file exits with ExitNotExist; other failures exit 1 with the
// message on standard error.
package agentrun

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/envutil"
)

// ExitNotExist is agentd-run --fs's exit code for a missing file.
const ExitNotExist = 3

// Setup is the first line agentd-run --exec reads.
type Setup struct {
	Path     string
	Args     []string
	Dir      string
	Env      []string // "KEY=value", applied after UnsetEnv
	UnsetEnv []string
	Files    []agent.File
}

// FS operations.
const (
	OpRead  = "read"
	OpStat  = "stat"
	OpLstat = "lstat"
	OpList  = "list"
)

// FSRequest is the line agentd-run --fs reads.
type FSRequest struct {
	Op   string
	Path string
	// Offset and Length choose a range of a read; zero Length reads to the
	// end.
	Offset int64 `json:",omitempty"`
	Length int64 `json:",omitempty"`
}

// ReadOK precedes a read's bytes, so the reader learns the file opened
// before any content arrives.
const ReadOK = "ok\n"

// FileInfo describes a file in a stat, lstat, or list answer.
type FileInfo struct {
	Name    string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
}

// Info returns i as an fs.FileInfo.
func (i FileInfo) Info() fs.FileInfo { return fileInfo{i} }

type fileInfo struct{ i FileInfo }

func (f fileInfo) Name() string       { return f.i.Name }
func (f fileInfo) Size() int64        { return f.i.Size }
func (f fileInfo) Mode() fs.FileMode  { return f.i.Mode }
func (f fileInfo) ModTime() time.Time { return f.i.ModTime }
func (f fileInfo) IsDir() bool        { return f.i.Mode.IsDir() }
func (f fileInfo) Sys() any           { return nil }

func infoOf(fi fs.FileInfo) FileInfo {
	return FileInfo{Name: fi.Name(), Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime()}
}

// ReadLine reads one line from r without reading past it, so whatever
// follows stays for the process r is handed to.
func ReadLine(r io.Reader) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return line, nil
			}
			line = append(line, b[0])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
	}
}

// WriteFiles writes setup files at their paths, creating parents.
func WriteFiles(files []agent.File) error {
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(f.Path, f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ApplyEnv applies setup's environment changes to this process.
func ApplyEnv(s Setup) error {
	for _, k := range s.UnsetEnv {
		if err := os.Unsetenv(k); err != nil {
			return err
		}
	}
	for k, v := range envutil.Map(s.Env) {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// ServeFS answers one FSRequest read from r on w. It returns an error
// matching fs.ErrNotExist for a missing file.
func ServeFS(r io.Reader, w io.Writer) error {
	line, err := ReadLine(r)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	var req FSRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	switch req.Op {
	case OpRead:
		f, err := os.Open(req.Path)
		if err != nil {
			return err
		}
		defer f.Close()
		if req.Offset > 0 {
			if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
				return err
			}
		}
		var src io.Reader = f
		if req.Length > 0 {
			src = io.LimitReader(f, req.Length)
		}
		bw := bufio.NewWriter(w)
		if _, err := bw.WriteString(ReadOK); err != nil {
			return err
		}
		if _, err := io.Copy(bw, src); err != nil {
			return err
		}
		return bw.Flush()
	case OpStat, OpLstat:
		stat := os.Stat
		if req.Op == OpLstat {
			stat = os.Lstat
		}
		fi, err := stat(req.Path)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(infoOf(fi))
	case OpList:
		entries, err := os.ReadDir(req.Path)
		if err != nil {
			return err
		}
		out := make([]FileInfo, 0, len(entries))
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				continue // removed since listing
			}
			out = append(out, infoOf(fi))
		}
		return json.NewEncoder(w).Encode(out)
	}
	return fmt.Errorf("unknown operation %q", req.Op)
}
