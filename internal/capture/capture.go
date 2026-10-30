// Package capture records the files, directories and symbolic links read
// through a file system, and writes a copy of exactly those entries
// elsewhere. gdiskmap uses it to capture the parts of sysfs and /dev it
// reads, so a run can be replayed (and tested) on another machine.
package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Recorder is an fs.FS that delegates to another file system and records
// every entry read through it.
type Recorder struct {
	fsys fs.ReadLinkFS

	mu    sync.Mutex
	files map[string][]byte // nil content: seen by Lstat only.
	dirs  map[string]bool
	links map[string]string
}

// NewRecorder returns a Recorder reading from fsys.
func NewRecorder(fsys fs.ReadLinkFS) *Recorder {
	return &Recorder{
		fsys:  fsys,
		files: make(map[string][]byte),
		dirs:  make(map[string]bool),
		links: make(map[string]string),
	}
}

// Open implements fs.FS. Entries opened directly are not recorded; callers
// should read through ReadFile and ReadDir.
func (r *Recorder) Open(name string) (fs.File, error) {
	return r.fsys.Open(name)
}

// ReadFile implements fs.ReadFileFS.
func (r *Recorder) ReadFile(name string) ([]byte, error) {
	b, err := fs.ReadFile(r.fsys, name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[name] = b
	return b, nil
}

// ReadDir implements fs.ReadDirFS.
func (r *Recorder) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(r.fsys, name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirs[name] = true
	return entries, nil
}

// ReadLink implements fs.ReadLinkFS.
func (r *Recorder) ReadLink(name string) (string, error) {
	target, err := r.fsys.ReadLink(name)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links[name] = target
	return target, nil
}

// Lstat implements fs.ReadLinkFS. It records the existence of the entry, so
// that, for example, a device node checked for existence is reproduced as an
// empty file.
func (r *Recorder) Lstat(name string) (fs.FileInfo, error) {
	info, err := r.fsys.Lstat(name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case info.IsDir():
		r.dirs[name] = true
	case info.Mode()&fs.ModeSymlink != 0:
		// The target is recorded when the link is read; record it now in
		// case it never is.
		if _, ok := r.links[name]; !ok {
			target, err := r.fsys.ReadLink(name)
			if err != nil {
				return nil, err
			}
			r.links[name] = target
		}
	default:
		if _, ok := r.files[name]; !ok {
			r.files[name] = nil
		}
	}
	return info, nil
}

// WriteTo writes every recorded entry under dir, which must be empty or not
// exist. Files are written with the content read; symbolic links keep their
// original (relative) targets so the copy resolves the same way.
func (r *Recorder) WriteTo(dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for name := range r.dirs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(name)), 0o755); err != nil {
			return err
		}
	}
	for name, content := range r.files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return err
		}
	}
	for name, target := range r.links {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(target, p); err != nil {
			return err
		}
	}
	return nil
}
