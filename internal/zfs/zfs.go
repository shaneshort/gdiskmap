// Package zfs reads pool membership from `zpool status -j`, the JSON output
// added in OpenZFS 2.3.
package zfs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/shaneshort/gdiskmap/internal/natsort"
)

// ErrUnsupported is returned by Status when the installed zpool does not
// support JSON output (OpenZFS older than 2.3).
var ErrUnsupported = errors.New("zpool does not support JSON output (OpenZFS 2.3 or later is required)")

// Vdev is a leaf vdev (a disk or file) of a pool.
type Vdev struct {
	Pool string `json:"pool"`
	// Class is the allocation class the vdev belongs to: "" for normal data
	// vdevs, or "log", "cache", "spare", "special" or "dedup".
	Class string `json:"class,omitempty"`
	// Parent is the slash-separated path of interior vdevs above this one,
	// e.g. "mirror-0" or "mirror-0/replacing-1". It is empty for a leaf at
	// the top level of its class.
	Parent string `json:"parent,omitempty"`
	// Name is the vdev name as zpool status prints it. For a vdev ZFS cannot
	// open this is its GUID.
	Name string `json:"name"`
	GUID string `json:"guid"`
	// Path is the device path ZFS uses, or last used, for the vdev.
	Path  string `json:"path,omitempty"`
	State string `json:"state"`
}

// Location returns the vdev's position within its pool, e.g. "mirror-0" or
// "cache", for display.
func (v Vdev) Location() string {
	return strings.Join(slices.DeleteFunc([]string{v.Class, v.Parent}, func(s string) bool { return s == "" }), "/")
}

// classKeys maps the pool-level JSON keys holding vdev trees to the class
// names used in Vdev.Class.
var classKeys = []struct{ key, class string }{
	{"vdevs", ""},
	{"special", "special"},
	{"dedup", "dedup"},
	{"logs", "log"},
	{"l2cache", "cache"},
	{"spares", "spare"},
}

type statusJSON struct {
	Pools map[string]map[string]json.RawMessage `json:"pools"`
}

type vdevJSON struct {
	Name     string              `json:"name"`
	VdevType string              `json:"vdev_type"`
	GUID     string              `json:"guid"`
	Path     string              `json:"path"`
	State    string              `json:"state"`
	Vdevs    map[string]vdevJSON `json:"vdevs"`
}

// Run runs `zpool status -j` using the zpool binary at zpoolPath and returns
// its raw output.
func Run(ctx context.Context, zpoolPath string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, zpoolPath, "status", "-j")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "invalid option") {
			return nil, ErrUnsupported
		}
		return nil, fmt.Errorf("running zpool status: %w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

// Parse decodes `zpool status -j` output into leaf vdevs, ordered by pool,
// class and position.
func Parse(data []byte) ([]Vdev, error) {
	var status statusJSON
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("parsing zpool status JSON: %w", err)
	}
	var vdevs []Vdev
	for _, poolName := range sortedKeys(status.Pools) {
		pool := status.Pools[poolName]
		for _, ck := range classKeys {
			raw, ok := pool[ck.key]
			if !ok {
				continue
			}
			var tree map[string]vdevJSON
			if err := json.Unmarshal(raw, &tree); err != nil {
				return nil, fmt.Errorf("parsing pool %s %s: %w", poolName, ck.key, err)
			}
			for _, name := range sortedKeys(tree) {
				v := tree[name]
				if ck.key == "vdevs" && name == poolName && v.VdevType != "disk" && v.VdevType != "file" {
					// The data class has a root vdev named after the pool;
					// its children are the top-level vdevs.
					vdevs = collectLeaves(vdevs, Vdev{Pool: poolName}, nil, v.Vdevs)
					continue
				}
				vdevs = collectLeaves(vdevs, Vdev{Pool: poolName, Class: ck.class}, nil, map[string]vdevJSON{name: v})
			}
		}
	}
	return vdevs, nil
}

// collectLeaves appends the leaves of tree to dst. template supplies the pool
// and class; parents holds the names of the interior vdevs above tree.
func collectLeaves(dst []Vdev, template Vdev, parents []string, tree map[string]vdevJSON) []Vdev {
	for _, name := range sortedKeys(tree) {
		v := tree[name]
		if len(v.Vdevs) > 0 {
			dst = collectLeaves(dst, template, append(slices.Clip(parents), name), v.Vdevs)
			continue
		}
		leaf := template
		leaf.Parent = strings.Join(parents, "/")
		leaf.Name = cmp.Or(v.Name, name)
		leaf.GUID = v.GUID
		leaf.Path = v.Path
		leaf.State = v.State
		dst = append(dst, leaf)
	}
	return dst
}

// sortedKeys returns m's keys in natural order, so that "mirror-10" sorts
// after "mirror-9".
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, natsort.Compare)
	return keys
}
