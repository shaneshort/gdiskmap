package capture

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

func TestRecorderRoundTrip(t *testing.T) {
	src := fstest.MapFS{
		"sys/devices/host0/proc_name": {Data: []byte("mpt2sas\n")},
		"sys/devices/host0/unread":    {Data: []byte("x")},
		"sys/class/scsi_host/host0":   {Data: []byte("../../devices/host0"), Mode: fs.ModeSymlink | 0o777},
		"dev/sda":                     {Data: nil},
	}
	r := NewRecorder(src)
	if _, err := fs.ReadDir(r, "sys/class/scsi_host"); err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	if _, err := fs.Lstat(r, "sys/class/scsi_host/host0"); err != nil {
		t.Fatalf("Lstat() error: %v", err)
	}
	if _, err := fs.ReadFile(r, "sys/devices/host0/proc_name"); err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if _, err := fs.Lstat(r, "dev/sda"); err != nil {
		t.Fatalf("Lstat() error: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "dump")
	if err := r.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}
	// The copy resolves the symbolic link the same way as the original.
	got, err := os.ReadFile(filepath.Join(dir, "sys/class/scsi_host/host0/proc_name"))
	if err != nil {
		t.Fatalf("reading through the copied link: %v", err)
	}
	if diff := cmp.Diff("mpt2sas\n", string(got)); diff != "" {
		t.Errorf("copied file mismatch (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(filepath.Join(dir, "dev/sda")); err != nil {
		t.Errorf("device node seen by Lstat was not reproduced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sys/devices/host0/unread")); err == nil {
		t.Error("unread file was copied, want only entries that were read")
	}
	if err := r.WriteTo(dir); err == nil {
		t.Error("WriteTo(non-empty dir) error = nil, want error")
	}
}

func TestRecorderListedEntries(t *testing.T) {
	src := fstest.MapFS{
		"d/link":        {Data: []byte("../target"), Mode: fs.ModeSymlink | 0o777},
		"d/sub/deep":    {Data: []byte("not listed")},
		"d/file":        {Data: []byte("content")},
		"d/read":        {Data: []byte("read")},
		"elsewhere/new": {Data: []byte("x")},
	}
	r := NewRecorder(src)
	if _, err := fs.ReadFile(r, "d/read"); err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if _, err := fs.ReadDir(r, "d"); err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "dump")
	if err := r.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}
	// Every listed entry exists in the copy, with its type; only content
	// that was read is copied.
	if target, err := os.Readlink(filepath.Join(dir, "d/link")); err != nil || target != "../target" {
		t.Errorf("listed link = %q, %v, want ../target", target, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "d/sub")); err != nil || !info.IsDir() {
		t.Errorf("listed directory not reproduced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d/sub/deep")); err == nil {
		t.Error("entry of an unlisted directory was copied")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "d/file")); err != nil || len(got) != 0 {
		t.Errorf("listed file = %q, %v, want an empty file", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "d/read")); err != nil || string(got) != "read" {
		t.Errorf("read file = %q, %v, want its content kept", got, err)
	}
}
