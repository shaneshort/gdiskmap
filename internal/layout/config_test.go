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

func TestSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gdiskmap.toml")
	c := &Config{}
	if err := c.SetLabel(0x1, 0, "A1"); err != nil {
		t.Fatalf("SetLabel() error: %v", err)
	}
	backup, err := c.Save(path)
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if backup != "" {
		t.Errorf("Save() of a new file returned backup %q, want none", backup)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}

	if err := c.SetLabel(0x1, 1, "A2"); err != nil {
		t.Fatalf("SetLabel() error: %v", err)
	}
	backup, err = c.Save(path)
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("reading backup: %v", err)
	}
	if string(saved) != string(first) {
		t.Errorf("backup content = %q, want %q", saved, first)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got, _ := loaded.Label(0x1, 1); got != "A2" {
		t.Errorf("Load() label = %q, want %q", got, "A2")
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
