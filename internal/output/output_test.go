package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gdiskmap/internal/inventory"
	"gdiskmap/internal/sysfs"
	"gdiskmap/internal/zfs"
)

func testInventory() *inventory.Inventory {
	return &inventory.Inventory{
		PoolsKnown: true,
		Slots: []inventory.Slot{
			{
				Label: "11", DefaultLabel: "c0:s0",
				Disk: &inventory.Disk{
					Device: sysfs.Device{Block: "sda", Model: "ST2000NX0433", Serial: "S460S1L7", Size: 2000398934016},
					Vdevs:  []zfs.Vdev{{Pool: "archive", Parent: "mirror-0", State: "ONLINE"}},
				},
				LED: &inventory.LEDState{Identify: true},
			},
			{Label: "c0:s1", DefaultLabel: "c0:s1"},
			{
				Label: "c0:s4", DefaultLabel: "c0:s4",
				Disk: &inventory.Disk{
					Device:     sysfs.Device{Block: "sdh", Model: "ST2000NX0433", Serial: "S460RYD7", Size: 2000398934016},
					LinkErrors: 10924,
				},
				LED: &inventory.LEDState{},
			},
		},
		Missing: []zfs.Vdev{{Pool: "rpool", Parent: "mirror-0", Name: "5587783532978086300", State: "UNAVAIL", Path: "/dev/disk/by-id/ata-INTEL-part3"}},
	}
}

func TestTable(t *testing.T) {
	var buf bytes.Buffer
	if err := Table(&buf, testInventory()); err != nil {
		t.Fatalf("Table() error: %v", err)
	}
	want := `BAY    SLOT   DEVICE  MODEL         SERIAL    SIZE  POOL     VDEV      STATE   LED     LINK ERRS
11     c0:s0  sda     ST2000NX0433  S460S1L7  2.0T  archive  mirror-0  ONLINE  locate
c0:s1  c0:s1  -       (empty)       -         -     -        -         -       -
c0:s4  c0:s4  sdh     ST2000NX0433  S460RYD7  2.0T  -        -         -       off     10924

Pool members not found on any device:
POOL   VDEV      NAME                 STATE    LAST PATH
rpool  mirror-0  5587783532978086300  UNAVAIL  /dev/disk/by-id/ata-INTEL-part3
`
	if got := buf.String(); got != want {
		t.Errorf("Table() =\n%s\nwant:\n%s", got, want)
	}
}

func TestTableWithoutCustomLabels(t *testing.T) {
	inv := testInventory()
	inv.Slots[0].Label = inv.Slots[0].DefaultLabel
	var buf bytes.Buffer
	if err := Table(&buf, inv); err != nil {
		t.Fatalf("Table() error: %v", err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); strings.Contains(first, "SLOT") {
		t.Errorf("Table() header = %q, want no SLOT column without custom labels", first)
	}
}

func TestTableWithoutPools(t *testing.T) {
	inv := testInventory()
	inv.PoolsKnown = false
	var buf bytes.Buffer
	if err := Table(&buf, inv); err != nil {
		t.Fatalf("Table() error: %v", err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); strings.Contains(first, "POOL") {
		t.Errorf("Table() header = %q, want no pool columns when pools are unknown", first)
	}
}

func TestTableWithoutLEDState(t *testing.T) {
	inv := testInventory()
	for i := range inv.Slots {
		inv.Slots[i].LED = nil
	}
	var buf bytes.Buffer
	if err := Table(&buf, inv); err != nil {
		t.Fatalf("Table() error: %v", err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); strings.Contains(first, "LED") {
		t.Errorf("Table() header = %q, want no LED column when no LED state was read", first)
	}
}

func TestJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, testInventory()); err != nil {
		t.Fatalf("JSON() error: %v", err)
	}
	var got inventory.Inventory
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("JSON() output does not decode: %v", err)
	}
	if len(got.Slots) != 3 || got.Slots[0].Disk.Block != "sda" || got.Slots[0].Disk.Vdevs[0].Pool != "archive" {
		t.Errorf("JSON() round trip = %+v, want the original slots", got.Slots)
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0B"},
		{999, "999B"},
		{1000, "1.0K"},
		{1000204886016, "1.0T"},
		{2000398934016, "2.0T"},
		{240057409536, "240.1G"},
	}
	for _, tc := range tests {
		if got := HumanSize(tc.n); got != tc.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
