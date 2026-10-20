package agent

import (
	"io"
	"io/fs"
	"os"
)

// FileSystem reads files on the machine where a Spawner runs the
// harnesses, for providers that read harness files directly, such as
// session transcripts. Paths are native to that machine. A missing file
// fails with an error matching fs.ErrNotExist.
type FileSystem interface {
	Open(path string) (io.ReadCloser, error)
	Stat(path string) (fs.FileInfo, error)
	// Lstat is Stat without following a final symbolic link.
	Lstat(path string) (fs.FileInfo, error)
	// ReadDir lists a directory, sorted by name.
	ReadDir(path string) ([]fs.DirEntry, error)
}

// FileSystemSpawner is implemented by spawners whose harnesses run on
// another machine and that can read files there.
type FileSystemSpawner interface {
	Spawner
	FileSystem() FileSystem
}

// LocalFileSystem is this machine's file system.
var LocalFileSystem FileSystem = localFileSystem{}

// FileSystemOf returns the file system where sp runs harnesses: its own
// when it implements FileSystemSpawner, else LocalFileSystem.
func FileSystemOf(sp Spawner) FileSystem {
	if f, ok := sp.(FileSystemSpawner); ok {
		if fsys := f.FileSystem(); fsys != nil {
			return fsys
		}
	}
	return LocalFileSystem
}

type localFileSystem struct{}

func (localFileSystem) Open(path string) (io.ReadCloser, error)    { return os.Open(path) }
func (localFileSystem) Stat(path string) (fs.FileInfo, error)      { return os.Stat(path) }
func (localFileSystem) Lstat(path string) (fs.FileInfo, error)     { return os.Lstat(path) }
func (localFileSystem) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
