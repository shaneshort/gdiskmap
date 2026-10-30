package inventory

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"gdiskmap/internal/layout"
	"gdiskmap/internal/mpt"
	"gdiskmap/internal/sysfs"
	"gdiskmap/internal/zfs"
)

const (
	encl0 = 0x500605b0013ca580
	encl1 = 0x500605b0071f24b0
)

func phys(errs map[int]uint64) []sysfs.Phy {
	var ps []sysfs.Phy
	for n := range 8 {
		ps = append(ps, sysfs.Phy{Number: n, InvalidDwords: errs[n]})
	}
	return ps
}

// testHosts models two controllers: host 0 (listed second by sysfs, but IOC
// 0) with two disks and a bad link on phy 7, and host 1 whose firmware
// reports two disks in slot 30.
func testHosts() []sysfs.Host {
	return []sysfs.Host{
		{
			Number: 5, Driver: sysfs.DriverSAS2, IOC: 1, SASAddress: encl1, Phys: phys(nil),
			Devices: []sysfs.Device{
				{EndDevice: "end_device-5:0", EnclosureID: encl1, Bay: 30, Phys: []int{2}, Handle: 0xb, HasHandle: true, Block: "sdm", Serial: "S2PWNX0J610092K"},
				{EndDevice: "end_device-5:1", EnclosureID: encl1, Bay: 30, Phys: []int{6}, Handle: 0xc, HasHandle: true, Block: "sdn", Serial: "S2PWNX0J609986X"},
				{EndDevice: "end_device-5:2", EnclosureID: encl1, Bay: -1, Phys: []int{3}, Handle: 0xd, HasHandle: true, Block: "sdo", Serial: "LOST"},
			},
		},
		{
			Number: 0, Driver: sysfs.DriverSAS2, IOC: 0, SASAddress: encl0, Phys: phys(map[int]uint64{7: 10924}),
			Devices: []sysfs.Device{
				{EndDevice: "end_device-0:0", EnclosureID: encl0, Bay: 4, Phys: []int{7}, Handle: 0x9, HasHandle: true, Block: "sdh", Serial: "S460RYD7", WWID: "naa.5000c500980611fb"},
				{EndDevice: "end_device-0:1", EnclosureID: encl0, Bay: 0, Phys: []int{3}, Handle: 0xa, HasHandle: true, Block: "sda", Serial: "S460S1L7", WWID: "naa.5000c5009804f337"},
			},
		},
	}
}

// fakeWholeDisk resolves the by-id paths used in the tests.
func fakeWholeDisk(devPath string) (string, error) {
	switch devPath {
	case "/dev/disk/by-id/scsi-35000c5009804f337":
		return "sda", nil
	case "/dev/disk/by-id/ata-Samsung_SSD_850_EVO_1TB_S2PWNX0J610092K-part1":
		return "sdm", nil
	case "/dev/disk/by-id/nvme-onboard":
		return "nvme0n1", nil
	}
	return "", fs.ErrNotExist
}

func testInventory(t *testing.T) *Inventory {
	t.Helper()
	cfg := &layout.Config{}
	if err := cfg.SetLabel(encl0, 4, "front-5"); err != nil {
		t.Fatalf("SetLabel() error: %v", err)
	}
	return Build(Sources{
		Hosts: testHosts(),
		Enclosures: map[int][]mpt.Enclosure{
			0: {{Handle: 1, LogicalID: encl0, StartSlot: 0, NumSlots: 8}, {Handle: 2, LogicalID: 0x99, NumSlots: 0}},
		},
		Vdevs: []zfs.Vdev{
			{Pool: "archive", Parent: "mirror-0", Name: "scsi-35000c5009804f337", Path: "/dev/disk/by-id/scsi-35000c5009804f337", State: "ONLINE"},
			{Pool: "storage", Parent: "mirror-3", Name: "ata-Samsung_SSD_850_EVO_1TB_S2PWNX0J610092K-part1", Path: "/dev/disk/by-id/ata-Samsung_SSD_850_EVO_1TB_S2PWNX0J610092K-part1", State: "ONLINE"},
			{Pool: "rpool", Parent: "mirror-0", Name: "5587783532978086300", Path: "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3", State: "UNAVAIL"},
			{Pool: "rpool", Parent: "mirror-0", Name: "nvme-onboard", Path: "/dev/disk/by-id/nvme-onboard", State: "ONLINE"},
		},
		WholeDisk: fakeWholeDisk,
		Layout:    cfg,
	})
}

func TestBuild(t *testing.T) {
	inv := testInventory(t)

	type row struct {
		Label, Default, Block string
		Pools                 int
		LinkErrors            uint64
	}
	var got []row
	for _, s := range inv.Slots {
		r := row{Label: s.Label, Default: s.DefaultLabel}
		if s.Disk != nil {
			r.Block, r.Pools, r.LinkErrors = s.Disk.Block, len(s.Disk.Vdevs), s.Disk.LinkErrors
		}
		got = append(got, r)
	}
	// Controller c0 has a firmware enclosure list, so all eight slots
	// appear; c1 does not, so only its occupied slot is known.
	want := []row{
		{Label: "c0:s0", Default: "c0:s0", Block: "sda", Pools: 1},
		{Label: "c0:s1", Default: "c0:s1"},
		{Label: "c0:s2", Default: "c0:s2"},
		{Label: "c0:s3", Default: "c0:s3"},
		{Label: "front-5", Default: "c0:s4", Block: "sdh", LinkErrors: 10924},
		{Label: "c0:s5", Default: "c0:s5"},
		{Label: "c0:s6", Default: "c0:s6"},
		{Label: "c0:s7", Default: "c0:s7"},
		{Label: "c1:s30", Default: "c1:s30", Block: "sdm", Pools: 1},
		{Label: "c1:s30", Default: "c1:s30", Block: "sdn"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Build() slots mismatch (-want +got):\n%s", diff)
	}

	if len(inv.Unplaced) != 1 || inv.Unplaced[0].Disk.Block != "sdo" {
		t.Errorf("Build() unplaced = %+v, want only sdo", inv.Unplaced)
	}
	if len(inv.Missing) != 1 || inv.Missing[0].Name != "5587783532978086300" {
		t.Errorf("Build() missing = %+v, want only the UNAVAIL rpool member", inv.Missing)
	}
	if len(inv.Warnings) != 2 ||
		!strings.Contains(inv.Warnings[0], "c1:s30") ||
		!strings.Contains(inv.Warnings[1], "sdo") {
		t.Errorf("Build() warnings = %q, want the duplicate slot and the unplaced disk", inv.Warnings)
	}
	if c := inv.Controllers; len(c) != 2 || c[0].Host != 0 || c[1].Host != 5 {
		t.Errorf("Build() controllers = %+v, want host 0 then host 5 (ordered by IOC)", c)
	}
}

func TestBuildMultipleEnclosures(t *testing.T) {
	inv := Build(Sources{
		Hosts: []sysfs.Host{{Number: 0, Driver: sysfs.DriverSAS3, IOC: 0}},
		Enclosures: map[int][]mpt.Enclosure{0: {
			{Handle: 1, LogicalID: 1, StartSlot: 0, NumSlots: 1},
			{Handle: 2, LogicalID: 2, StartSlot: 0, NumSlots: 1},
		}},
	})
	var got []string
	for _, s := range inv.Slots {
		got = append(got, s.DefaultLabel)
	}
	if diff := cmp.Diff([]string{"c0:e0:s0", "c0:e1:s0"}, got); diff != "" {
		t.Errorf("Build() default labels mismatch (-want +got):\n%s", diff)
	}
}

func TestSelect(t *testing.T) {
	inv := testInventory(t)
	tests := []struct {
		targets []string
		want    []string // Default labels, with the block device for occupied slots.
	}{
		{[]string{"front-5"}, []string{"c0:s4 sdh"}},
		{[]string{"c0:s4"}, []string{"c0:s4 sdh"}},
		{[]string{"sda"}, []string{"c0:s0 sda"}},
		{[]string{"/dev/sdn"}, []string{"c1:s30 sdn"}},
		{[]string{"/dev/disk/by-id/scsi-35000c5009804f337"}, []string{"c0:s0 sda"}},
		{[]string{"s460ryd7"}, []string{"c0:s4 sdh"}},
		{[]string{"0x5000c5009804f337"}, []string{"c0:s0 sda"}},
		{[]string{"sdo"}, []string{"c1:sdo sdo"}},
		{[]string{"sda", "c0:s0", "c0:s1"}, []string{"c0:s0 sda", "c0:s1"}},
		{[]string{"empty"}, []string{"c0:s1", "c0:s2", "c0:s3", "c0:s5", "c0:s6", "c0:s7"}},
		{[]string{"occupied"}, []string{"c0:s0 sda", "c0:s4 sdh", "c1:s30 sdm", "c1:s30 sdn", "c1:sdo sdo"}},
	}
	for _, tc := range tests {
		slots, err := inv.Select(tc.targets, fakeWholeDisk)
		if err != nil {
			t.Errorf("Select(%q) error: %v", tc.targets, err)
			continue
		}
		var got []string
		for _, s := range slots {
			d := s.DefaultLabel
			if s.Disk != nil {
				d += " " + s.Disk.Block
			}
			got = append(got, d)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("Select(%q) mismatch (-want +got):\n%s", tc.targets, diff)
		}
	}
	all, err := inv.Select([]string{"all"}, nil)
	if err != nil || len(all) != len(inv.Slots)+len(inv.Unplaced) {
		t.Errorf("Select(all) = %d slots, %v; want %d", len(all), err, len(inv.Slots)+len(inv.Unplaced))
	}
}

func TestSelectErrors(t *testing.T) {
	inv := testInventory(t)
	for _, target := range []string{"c1:s30", "nothing", "/dev/disk/by-id/absent"} {
		if got, err := inv.Select([]string{target}, fakeWholeDisk); err == nil {
			t.Errorf("Select(%q) = %v, want error", target, got)
		}
	}
	if _, err := inv.Select([]string{"/dev/disk/by-id/x"}, nil); err == nil {
		t.Error("Select(by-id path) without a resolver: error = nil, want error")
	}
	_, err := inv.Select([]string{"/dev/disk/by-id/absent"}, fakeWholeDisk)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Select(missing path) error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestNewLEDState(t *testing.T) {
	got := NewLEDState(mpt.StatusIdentify | mpt.StatusPredictedFault | 0x1)
	want := &LEDState{Identify: true, Fault: true, Raw: 0x00020041}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("NewLEDState() mismatch (-want +got):\n%s", diff)
	}
}
