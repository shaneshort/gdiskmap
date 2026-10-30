// Package layout maps controller slots to front-panel bay labels and builds
// that mapping interactively through calibration.
package layout

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

// DefaultPath is the configuration file used when none is specified.
const DefaultPath = "/etc/gdiskmap.toml"

// ReservedLabels are target keywords that cannot be used as bay labels.
var ReservedLabels = []string{"all", "occupied", "empty"}

// Config holds bay labels and unusable-bay marks keyed by enclosure logical
// ID and slot number. The zero value is an empty configuration.
type Config struct {
	enclosures map[uint64]*enclosure
}

type enclosure struct {
	name   string
	labels map[int]string
	// unusable maps slots marked as unusable (e.g. a faulty backplane
	// connector) to the reason, which may be empty.
	unusable map[int]string
}

// fileFormat is the TOML representation of Config.
type fileFormat struct {
	Enclosures []struct {
		ID       string            `toml:"id"`
		Name     string            `toml:"name"`
		Slots    map[string]string `toml:"slots"`
		Unusable map[string]string `toml:"unusable"`
	} `toml:"enclosure"`
}

// Load reads the configuration at path. A missing file yields an empty
// configuration, because the layout config is optional.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates a configuration.
func Parse(data string) (*Config, error) {
	var f fileFormat
	md, err := toml.Decode(data, &f)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("unknown key %q", undecoded[0].String())
	}
	c := &Config{}
	for _, e := range f.Enclosures {
		id, err := ParseID(e.ID)
		if err != nil {
			return nil, err
		}
		if _, dup := c.enclosures[id]; dup {
			return nil, fmt.Errorf("enclosure %s listed twice", e.ID)
		}
		if err := c.SetName(id, e.Name); err != nil {
			return nil, fmt.Errorf("enclosure %s: %w", e.ID, err)
		}
		for key, label := range e.Slots {
			slot, err := parseSlot(key)
			if err != nil {
				return nil, fmt.Errorf("enclosure %s: %w", e.ID, err)
			}
			if err := c.SetLabel(id, slot, label); err != nil {
				return nil, fmt.Errorf("enclosure %s slot %d: %w", e.ID, slot, err)
			}
		}
		for key, reason := range e.Unusable {
			slot, err := parseSlot(key)
			if err != nil {
				return nil, fmt.Errorf("enclosure %s: %w", e.ID, err)
			}
			if err := c.SetUnusable(id, slot, reason); err != nil {
				return nil, fmt.Errorf("enclosure %s slot %d: %w", e.ID, slot, err)
			}
		}
	}
	return c, nil
}

func parseSlot(key string) (int, error) {
	slot, err := strconv.Atoi(key)
	if err != nil || slot < 0 {
		return 0, fmt.Errorf("invalid slot number %q", key)
	}
	return slot, nil
}

// ParseID parses an enclosure logical ID written as hexadecimal, with or
// without a "0x" prefix.
func ParseID(s string) (uint64, error) {
	id, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid enclosure id %q", s)
	}
	return id, nil
}

// FormatID formats an enclosure logical ID as ParseID accepts it.
func FormatID(id uint64) string {
	return fmt.Sprintf("0x%016x", id)
}

// Label returns the label of a slot, if one is configured. A nil Config has
// no labels.
func (c *Config) Label(id uint64, slot int) (string, bool) {
	if c == nil {
		return "", false
	}
	e, ok := c.enclosures[id]
	if !ok {
		return "", false
	}
	l, ok := e.labels[slot]
	return l, ok
}

// Name returns the configured name of an enclosure, or "".
func (c *Config) Name(id uint64) string {
	if e, ok := c.enclosures[id]; ok {
		return e.name
	}
	return ""
}

// SetName sets the name of an enclosure.
func (c *Config) SetName(id uint64, name string) error {
	if err := validateText("name", name); err != nil {
		return err
	}
	c.enclosure(id).name = name
	return nil
}

// Unusable reports whether a slot is marked as unusable, and the reason
// given. A nil Config has no marks.
func (c *Config) Unusable(id uint64, slot int) (reason string, ok bool) {
	if c == nil {
		return "", false
	}
	e, found := c.enclosures[id]
	if !found {
		return "", false
	}
	reason, ok = e.unusable[slot]
	return reason, ok
}

// SetUnusable marks a slot as unusable. The reason may be empty.
func (c *Config) SetUnusable(id uint64, slot int, reason string) error {
	if err := validateText("reason", reason); err != nil {
		return err
	}
	c.enclosure(id).unusable[slot] = reason
	return nil
}

// ClearUnusable removes a slot's unusable mark, if any.
func (c *Config) ClearUnusable(id uint64, slot int) {
	if e, ok := c.enclosures[id]; ok {
		delete(e.unusable, slot)
	}
}

// SetLabel sets the label of a slot. Labels must be unique across the whole
// configuration, because they are used to address slots.
func (c *Config) SetLabel(id uint64, slot int, label string) error {
	if err := validateLabel(label); err != nil {
		return err
	}
	for eid, e := range c.enclosures {
		for s, l := range e.labels {
			if l == label && (eid != id || s != slot) {
				return fmt.Errorf("label %q is already used by enclosure %s slot %d", label, FormatID(eid), s)
			}
		}
	}
	c.enclosure(id).labels[slot] = label
	return nil
}

// ClearLabels removes every slot label of an enclosure, keeping its name and
// unusable marks.
func (c *Config) ClearLabels(id uint64) {
	clear(c.enclosure(id).labels)
}

// Labels returns every configured label with its enclosure and slot.
func (c *Config) Labels() map[uint64]map[int]string {
	out := make(map[uint64]map[int]string)
	if c == nil {
		return out
	}
	for id, e := range c.enclosures {
		out[id] = maps.Clone(e.labels)
	}
	return out
}

func (c *Config) enclosure(id uint64) *enclosure {
	if c.enclosures == nil {
		c.enclosures = make(map[uint64]*enclosure)
	}
	e, ok := c.enclosures[id]
	if !ok {
		e = &enclosure{labels: make(map[int]string), unusable: make(map[int]string)}
		c.enclosures[id] = e
	}
	return e
}

func validateLabel(label string) error {
	switch {
	case label == "":
		return errors.New("label is empty")
	case slices.Contains(ReservedLabels, label):
		return fmt.Errorf("label %q is a reserved target keyword", label)
	}
	return validateText("label", label)
}

// validateText rejects control characters, which the TOML encoding (see
// quote) does not escape.
func validateText(what, s string) error {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return fmt.Errorf("%s %q contains control characters", what, s)
	}
	return nil
}

// Encode returns the configuration in its TOML file format, with enclosures
// and slots in numeric order.
func (c *Config) Encode() string {
	var b strings.Builder
	b.WriteString("# gdiskmap layout: maps controller slots to front-panel bay labels.\n")
	b.WriteString("# Enclosure ids are enclosure logical IDs (for direct-attach backplanes,\n")
	b.WriteString("# the HBA's SAS address). Generated by `gdiskmap calibrate`; safe to edit.\n")
	ids := make([]uint64, 0, len(c.enclosures))
	for id := range c.enclosures {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		e := c.enclosures[id]
		fmt.Fprintf(&b, "\n[[enclosure]]\nid = %s\n", quote(FormatID(id)))
		if e.name != "" {
			fmt.Fprintf(&b, "name = %s\n", quote(e.name))
		}
		b.WriteString("\n  [enclosure.slots]\n")
		writeSlotTable(&b, e.labels)
		if len(e.unusable) > 0 {
			b.WriteString("\n  # Slots that must not be used, with the reason.\n  [enclosure.unusable]\n")
			writeSlotTable(&b, e.unusable)
		}
	}
	return b.String()
}

// writeSlotTable writes slot = "value" lines in slot order.
func writeSlotTable(b *strings.Builder, m map[int]string) {
	slots := slices.Sorted(maps.Keys(m))
	for _, s := range slots {
		fmt.Fprintf(b, "  %d = %s\n", s, quote(m[s]))
	}
}

// quote returns s as a TOML basic string. Labels, names and reasons never
// contain control characters (see validateText), so only quotes and
// backslashes need escaping.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Save writes the configuration to path. An existing file is first copied to
// a timestamped backup, whose path is returned ("" if there was no existing
// file).
func (c *Config) Save(path string) (backup string, err error) {
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		backup = fmt.Sprintf("%s.%s.bak", path, time.Now().Format("20060102T150405"))
		if err := os.WriteFile(backup, existing, 0o644); err != nil {
			return "", fmt.Errorf("backing up %s: %w", path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	// Write to a temporary file in the same directory and rename it over the
	// original, so a failed write never leaves a truncated configuration.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // Fails harmlessly after a successful rename.
	if _, err := tmp.WriteString(c.Encode()); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return backup, nil
}
