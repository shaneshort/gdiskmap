package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `
[[enclosure]]
id = "0x500605b0071f24b0"
name = "front"

  [enclosure.slots]
  0 = "11"
  3 = "8"
  10 = "front \"left\""

  [enclosure.unusable]
  2 = "connector clip broken"
  5 = ""

[[enclosure]]
id = "500605B0013CA580"
`

func TestParse(t *testing.T) {
	c, err := Parse(sampleConfig)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	tests := []struct {
		id     uint64
		slot   int
		want   string
		wantOK bool
	}{
		{0x500605b0071f24b0, 0, "11", true},
		{0x500605b0071f24b0, 10, `front "left"`, true},
		{0x500605b0071f24b0, 1, "", false},
		{0x500605b0013ca580, 0, "", false},
		{0x1, 0, "", false},
	}
	for _, tc := range tests {
		got, ok := c.Label(tc.id, tc.slot)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("Label(%#x, %d) = %q, %t, want %q, %t", tc.id, tc.slot, got, ok, tc.want, tc.wantOK)
		}
	}
	if got := c.Name(0x500605b0071f24b0); got != "front" {
		t.Errorf("Name() = %q, want %q", got, "front")
	}
	unusable := []struct {
		slot       int
		wantReason string
		wantOK     bool
	}{
		{2, "connector clip broken", true},
		{5, "", true},
		{0, "", false},
	}
	for _, tc := range unusable {
		reason, ok := c.Unusable(0x500605b0071f24b0, tc.slot)
		if reason != tc.wantReason || ok != tc.wantOK {
			t.Errorf("Unusable(slot %d) = %q, %t, want %q, %t", tc.slot, reason, ok, tc.wantReason, tc.wantOK)
		}
	}
}

func TestUnusableMarks(t *testing.T) {
	c := &Config{}
	if err := c.SetLabel(1, 0, "A"); err != nil {
		t.Fatalf("SetLabel() error: %v", err)
	}
	if err := c.SetUnusable(1, 3, "dead"); err != nil {
		t.Fatalf("SetUnusable() error: %v", err)
	}
	c.ClearLabels(1)
	if _, ok := c.Unusable(1, 3); !ok {
		t.Error("ClearLabels() removed an unusable mark, want it kept")
	}
	c.ClearUnusable(1, 3)
	c.ClearUnusable(2, 0) // Unknown enclosure: no effect.
	if _, ok := c.Unusable(1, 3); ok {
		t.Error("ClearUnusable() left the mark")
	}
	if err := c.SetUnusable(1, 3, "bad\nreason"); err == nil {
		t.Error("SetUnusable(control characters) error = nil, want error")
	}
	var nilConfig *Config
	if _, ok := nilConfig.Unusable(1, 3); ok {
		t.Error("nil Config Unusable() = true, want false")
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"bad id":          "[[enclosure]]\nid = \"xyz\"\n",
		"duplicate id":    "[[enclosure]]\nid = \"0x1\"\n[[enclosure]]\nid = \"1\"\n",
		"bad slot":        "[[enclosure]]\nid = \"0x1\"\n[enclosure.slots]\nx = \"1\"\n",
		"negative slot":   "[[enclosure]]\nid = \"0x1\"\n[enclosure.slots]\n-1 = \"1\"\n",
		"duplicate label": "[[enclosure]]\nid = \"0x1\"\n[enclosure.slots]\n0 = \"A\"\n1 = \"A\"\n",
		"reserved label":  "[[enclosure]]\nid = \"0x1\"\n[enclosure.slots]\n0 = \"all\"\n",
		"empty label":     "[[enclosure]]\nid = \"0x1\"\n[enclosure.slots]\n0 = \"\"\n",
		"unknown key":     "[[enclosure]]\nid = \"0x1\"\ncolour = \"red\"\n",
		"control in name": "[[enclosure]]\nid = \"0x1\"\nname = \"a\\tb\"\n",
		"bad unusable":    "[[enclosure]]\nid = \"0x1\"\n[enclosure.unusable]\nx = \"\"\n",
		"invalid TOML":    "[[enclosure]\n",
	}
	for name, in := range tests {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%s) error = nil, want error", name)
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	c, err := Parse(sampleConfig)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	encoded := c.Encode()
	// Slots are written in numeric order, not string order.
	if i, j := strings.Index(encoded, "  3 = "), strings.Index(encoded, "  10 = "); i < 0 || j < 0 || i > j {
		t.Errorf("Encode() slot order wrong:\n%s", encoded)
	}
	again, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse(Encode()) error: %v\n%s", err, encoded)
	}
	if got := again.Encode(); got != encoded {
		t.Errorf("Encode() not stable across a round trip:\n%s\nthen:\n%s", encoded, got)
	}
}

func TestUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gdiskmap.toml")
	setLabel := func(slot int, label string) func(*Config) error {
		return func(c *Config) error { return c.SetLabel(0x1, slot, label) }
	}
	backup, err := Update(path, setLabel(0, "A1"))
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if backup != "" {
		t.Errorf("Update() of a new file returned backup %q, want none", backup)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}

	// Two updates within the same second each keep their own backup.
	backup1, err := Update(path, setLabel(1, "A2"))
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	backup2, err := Update(path, setLabel(2, "A3"))
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if backup1 == backup2 {
		t.Fatalf("Update() reused backup name %q", backup1)
	}
	saved, err := os.ReadFile(backup1)
	if err != nil {
		t.Fatalf("reading backup: %v", err)
	}
	if string(saved) != string(first) {
		t.Errorf("first backup content = %q, want %q", saved, first)
	}

	// Each update starts from the file, so earlier changes are kept.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	for slot, want := range []string{"A1", "A2", "A3"} {
		if got, _ := loaded.Label(0x1, slot); got != want {
			t.Errorf("Load() slot %d label = %q, want %q", slot, got, want)
		}
	}

	// A failing change writes nothing.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if _, err := Update(path, setLabel(3, "A1")); err == nil {
		t.Error("Update(duplicate label) error = nil, want error")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("failed Update() changed the file:\n%s", after)
	}
	matches, err := filepath.Glob(path + ".*.bak")
	if err != nil {
		t.Fatalf("listing backups: %v", err)
	}
	if len(matches) != 2 {
		t.Errorf("found backups %q, want 2", matches)
	}
}

func TestLoadMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load(missing) error: %v", err)
	}
	if _, ok := c.Label(1, 0); ok {
		t.Error("Load(missing) returned labels, want an empty config")
	}
}
