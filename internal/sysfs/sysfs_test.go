package sysfs

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/shaneshort/gdiskmap/internal/capture"
)

// fakeSys builds an in-memory copy of the parts of sysfs and /dev that
// discovery reads, using the same directory layout and symbolic links as a
// real system.
type fakeSys struct {
	fsys fstest.MapFS
}

func newFakeSys(t *testing.T) *fakeSys {
	t.Helper()
	return &fakeSys{fsys: fstest.MapFS{}}
}

func (f *fakeSys) file(name, content string) {
	f.fsys[name] = &fstest.MapFile{Data: []byte(content)}
}

func (f *fakeSys) link(name, target string) {
	f.fsys[name] = &fstest.MapFile{Data: []byte(target), Mode: 0o777 | fs.ModeSymlink}
}

// fakeHost describes one SCSI host for fakeSys.addHost.
type fakeHost struct {
	number  int
	pci     string
	driver  string
	ioc     int
	sasAddr string
	phys    int
	// errors maps a phy number to its invalid dword count.
	errors  map[int]int
	devices []fakeDevice
}

// fakeDevice describes one end device with a single logical unit.
type fakeDevice struct {
	port    int
	phys    []int
	sasAddr string
	encl    string
	bay     string
	handle  string
	block   string
	vendor  string
	model   string
	serial  string
	wwid    string
	sectors string
	// parts lists partition numbers to create under the block device.
	parts []int
}

func (f *fakeSys) addHost(h fakeHost) {
	host := fmt.Sprintf("host%d", h.number)
	hostDir := path.Join("sys/devices/pci0000:00/0000:00:01.1", h.pci, host)
	classDir := path.Join(hostDir, "scsi_host", host)
	f.link(path.Join("sys/class/scsi_host", host), path.Join("../../devices/pci0000:00/0000:00:01.1", h.pci, host, "scsi_host", host))
	f.file(path.Join(classDir, "proc_name"), h.driver+"\n")
	f.file(path.Join(classDir, "unique_id"), fmt.Sprintf("%d\n", h.ioc))
	if h.driver != DriverSAS2 && h.driver != DriverSAS3 {
		return
	}
	f.file(path.Join(classDir, "version_fw"), "20.00.07.00\n")
	f.file(path.Join(classDir, "board_name"), "SAS9207-8i\n")
	f.file(path.Join(classDir, "host_sas_address"), h.sasAddr+"\n")

	for n := range h.phys {
		phy := fmt.Sprintf("phy-%d:%d", h.number, n)
		dir := path.Join(hostDir, phy, "sas_phy", phy)
		f.file(path.Join(dir, "invalid_dword_count"), fmt.Sprintf("%d\n", h.errors[n]))
		f.file(path.Join(dir, "running_disparity_error_count"), "0\n")
		f.file(path.Join(dir, "loss_of_dword_sync_count"), "0\n")
		f.file(path.Join(dir, "phy_reset_problem_count"), "0\n")
	}

	for i, d := range h.devices {
		port := fmt.Sprintf("port-%d:%d", h.number, d.port)
		end := fmt.Sprintf("end_device-%d:%d", h.number, d.port)
		portDir := path.Join(hostDir, port)
		for _, n := range d.phys {
			phy := fmt.Sprintf("phy-%d:%d", h.number, n)
			f.link(path.Join(portDir, phy), "../"+phy)
		}
		endDir := path.Join(portDir, end)
		attrs := path.Join(endDir, "sas_device", end)
		f.file(path.Join(attrs, "sas_address"), d.sasAddr+"\n")
		f.file(path.Join(attrs, "enclosure_identifier"), d.encl+"\n")
		f.file(path.Join(attrs, "bay_identifier"), d.bay+"\n")
		if d.block == "" {
			continue
		}
		lun := fmt.Sprintf("%d:0:%d:0", h.number, i)
		lunDir := path.Join(endDir, fmt.Sprintf("target%d:0:%d", h.number, i), lun)
		f.file(path.Join(lunDir, "vendor"), d.vendor+"\n")
		f.file(path.Join(lunDir, "model"), d.model+"\n")
		f.file(path.Join(lunDir, "wwid"), d.wwid+"\n")
		f.file(path.Join(lunDir, "sas_device_handle"), d.handle+"\n")
		f.file(path.Join(lunDir, "vpd_pg80"), vpdPage80(d.serial))
		blockDir := path.Join(lunDir, "block", d.block)
		f.file(path.Join(blockDir, "size"), d.sectors+"\n")
		rel := path.Join("../../devices/pci0000:00/0000:00:01.1", h.pci, host, port, end,
			fmt.Sprintf("target%d:0:%d", h.number, i), lun, "block", d.block)
		f.link(path.Join("sys/class/block", d.block), rel)
		f.file(path.Join("dev", d.block), "")
		for _, p := range d.parts {
			part := fmt.Sprintf("%s%d", d.block, p)
			f.file(path.Join(blockDir, part, "partition"), fmt.Sprintf("%d\n", p))
			f.link(path.Join("sys/class/block", part), path.Join(rel, part))
			f.file(path.Join("dev", part), "")
		}
	}
}

// vpdPage80 encodes serial as a VPD Unit Serial Number page, padded with
// spaces as some devices do.
func vpdPage80(serial string) string {
	body := serial + "    "
	return string([]byte{0, 0x80, byte(len(body) >> 8), byte(len(body))}) + body
}

// pveFixture models two of the reference host's controllers: a healthy
// SAS2008 and a SAS2308 whose firmware reported duplicate bay numbers, plus
// an AHCI host that discovery must ignore.
func pveFixture(t *testing.T) *fakeSys {
	t.Helper()
	f := newFakeSys(t)
	f.addHost(fakeHost{
		number: 0, pci: "0000:c1:00.0", driver: DriverSAS2, ioc: 0,
		sasAddr: "0x500605b0013ca580", phys: 8, errors: map[int]int{7: 10924},
		devices: []fakeDevice{
			{
				port: 0, phys: []int{7}, sasAddr: "0x5000c5009804f335", encl: "0x500605b0013ca580",
				bay: "0", handle: "0x0009", block: "sda", vendor: "SEAGATE", model: "ST2000NX0433",
				serial: "S460S1L7", wwid: "naa.5000c5009804f337", sectors: "3907029168",
			},
			{
				port: 1, phys: []int{0}, sasAddr: "0x4433221100000000", encl: "0x500605b0013ca580",
				bay: "3", handle: "0x000a", block: "sdb", vendor: "ATA", model: "CT2000BX500SSD1",
				serial: "2245E6823C14", wwid: "t10.ATA     CT2000BX500SSD1  2245E6823C14", sectors: "3907029168",
				parts: []int{1, 9},
			},
		},
	})
	f.addHost(fakeHost{
		number: 1, pci: "0000:01:00.0", driver: DriverSAS2, ioc: 1,
		sasAddr: "0x500605b0071f24b0", phys: 8,
		devices: []fakeDevice{
			{
				port: 0, phys: []int{2}, sasAddr: "0x4433221102000000", encl: "0x500605b0071f24b0",
				bay: "30", handle: "0x000b", block: "sdm", vendor: "ATA", model: "Samsung SSD 850",
				serial: "S2PWNX0J610092K", wwid: "naa.5002538d42122706", sectors: "1953525168",
			},
			{
				port: 1, phys: []int{6}, sasAddr: "0x4433221106000000", encl: "0x500605b0071f24b0",
				bay: "30", handle: "0x000c", block: "sdn", vendor: "ATA", model: "Samsung SSD 850",
				serial: "S2PWNX0J609986X", wwid: "naa.5002538d4212257a", sectors: "1953525168",
			},
		},
	})
	f.addHost(fakeHost{number: 2, pci: "0000:00:17.0", driver: "ahci", ioc: 2})
	f.link("dev/disk/by-id/ata-CT2000BX500SSD1_2245E6823C14-part1", "../../sdb1")
	f.link("dev/disk/by-id/scsi-35000c5009804f337", "../../sda")
	f.link("dev/disk/by-id/wwn-0xloop", "wwn-0xloop")
	return f
}

func TestDiscover(t *testing.T) {
	f := pveFixture(t)
	got, err := Discover(f.fsys)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	phys := func(errs map[int]uint64) []Phy {
		var ps []Phy
		for n := range 8 {
			ps = append(ps, Phy{Number: n, InvalidDwords: errs[n]})
		}
		return ps
	}
	want := []Host{
		{
			Number: 0, Driver: DriverSAS2, IOC: 0, PCIAddress: "0000:c1:00.0",
			Firmware: "20.00.07.00", Board: "SAS9207-8i", SASAddress: 0x500605b0013ca580,
			Phys: phys(map[int]uint64{7: 10924}),
			Devices: []Device{
				{
					EndDevice: "end_device-0:0", SASAddress: 0x5000c5009804f335, EnclosureID: 0x500605b0013ca580,
					Bay: 0, Phys: []int{7}, Handle: 0x9, HasHandle: true, Block: "sda", Vendor: "SEAGATE",
					Model: "ST2000NX0433", Serial: "S460S1L7", WWID: "naa.5000c5009804f337", Size: 2000398934016,
				},
				{
					EndDevice: "end_device-0:1", SASAddress: 0x4433221100000000, EnclosureID: 0x500605b0013ca580,
					Bay: 3, Phys: []int{0}, Handle: 0xa, HasHandle: true, Block: "sdb", Vendor: "ATA",
					Model: "CT2000BX500SSD1", Serial: "2245E6823C14",
					WWID: "t10.ATA     CT2000BX500SSD1  2245E6823C14", Size: 2000398934016,
				},
			},
		},
		{
			Number: 1, Driver: DriverSAS2, IOC: 1, PCIAddress: "0000:01:00.0",
			Firmware: "20.00.07.00", Board: "SAS9207-8i", SASAddress: 0x500605b0071f24b0,
			Phys: phys(nil),
			Devices: []Device{
				{
					EndDevice: "end_device-1:0", SASAddress: 0x4433221102000000, EnclosureID: 0x500605b0071f24b0,
					Bay: 30, Phys: []int{2}, Handle: 0xb, HasHandle: true, Block: "sdm", Vendor: "ATA",
					Model: "Samsung SSD 850", Serial: "S2PWNX0J610092K", WWID: "naa.5002538d42122706", Size: 1000204886016,
				},
				{
					EndDevice: "end_device-1:1", SASAddress: 0x4433221106000000, EnclosureID: 0x500605b0071f24b0,
					Bay: 30, Phys: []int{6}, Handle: 0xc, HasHandle: true, Block: "sdn", Vendor: "ATA",
					Model: "Samsung SSD 850", Serial: "S2PWNX0J609986X", WWID: "naa.5002538d4212257a", Size: 1000204886016,
				},
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Discover() mismatch (-want +got):\n%s", diff)
	}
}

func TestDiscoverUnknownBay(t *testing.T) {
	f := newFakeSys(t)
	f.addHost(fakeHost{
		number: 3, pci: "0000:41:00.0", driver: DriverSAS3, ioc: 0, sasAddr: "0x1", phys: 1,
		devices: []fakeDevice{{port: 0, phys: []int{0}, sasAddr: "0x2", encl: "0x1", bay: "-6"}},
	})
	got, err := Discover(f.fsys)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(got) != 1 || len(got[0].Devices) != 1 {
		t.Fatalf("Discover() = %+v, want one host with one device", got)
	}
	if d := got[0].Devices[0]; d.Bay != -1 || d.Block != "" {
		t.Errorf("Discover() device = %+v, want Bay -1 and no block device", d)
	}
}

func TestWholeDisk(t *testing.T) {
	f := pveFixture(t)
	tests := []struct {
		devPath string
		want    string
	}{
		{"/dev/disk/by-id/ata-CT2000BX500SSD1_2245E6823C14-part1", "sdb"},
		{"/dev/disk/by-id/scsi-35000c5009804f337", "sda"},
		{"/dev/sdb9", "sdb"},
		{"/dev/sdm", "sdm"},
	}
	for _, tc := range tests {
		got, err := WholeDisk(f.fsys, tc.devPath)
		if err != nil {
			t.Errorf("WholeDisk(%q) error: %v", tc.devPath, err)
			continue
		}
		if got != tc.want {
			t.Errorf("WholeDisk(%q) = %q, want %q", tc.devPath, got, tc.want)
		}
	}
}

// addDM adds a device-mapper device whose slaves are the given block
// devices, and a /dev/mapper link to it.
func (f *fakeSys) addDM(name, mapperName string, slaves ...string) {
	dir := path.Join("sys/devices/virtual/block", name)
	f.link(path.Join("sys/class/block", name), path.Join("../../devices/virtual/block", name))
	f.file(path.Join(dir, "size"), "0\n")
	for _, sl := range slaves {
		f.link(path.Join(dir, "slaves", sl), path.Join("../../../../../sys/class/block", sl))
	}
	f.file(path.Join("dev", name), "")
	f.link(path.Join("dev/mapper", mapperName), "../"+name)
}

func TestPhysicalDisks(t *testing.T) {
	f := pveFixture(t)
	// A multipath device over two paths (sdm and sdn), a partition on it,
	// and dm-crypt on a partition of sdb.
	f.addDM("dm-0", "mpatha", "sdm", "sdn")
	f.addDM("dm-1", "mpatha-part1", "dm-0")
	f.addDM("dm-2", "crypt", "sdb1")
	tests := []struct {
		devPath string
		want    []string
	}{
		{"/dev/sdb9", []string{"sdb"}},
		{"/dev/mapper/mpatha", []string{"sdm", "sdn"}},
		{"/dev/mapper/mpatha-part1", []string{"sdm", "sdn"}},
		{"/dev/mapper/crypt", []string{"sdb"}},
	}
	for _, tc := range tests {
		got, err := PhysicalDisks(f.fsys, tc.devPath)
		if err != nil {
			t.Errorf("PhysicalDisks(%q) error: %v", tc.devPath, err)
			continue
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("PhysicalDisks(%q) mismatch (-want +got):\n%s", tc.devPath, diff)
		}
	}
	if got, err := WholeDisk(f.fsys, "/dev/mapper/mpatha-part1"); err != nil || got != "dm-1" {
		t.Errorf("WholeDisk(mpatha-part1) = %q, %v, want dm-1", got, err)
	}
}

func TestPhysicalDisksCycle(t *testing.T) {
	f := newFakeSys(t)
	f.addDM("dm-0", "a", "dm-1")
	f.addDM("dm-1", "b", "dm-0")
	if got, err := PhysicalDisks(f.fsys, "/dev/mapper/a"); err == nil {
		t.Errorf("PhysicalDisks(cycle) = %q, want error", got)
	}
}

func TestDiscoverExpander(t *testing.T) {
	f := pveFixture(t)
	f.file("sys/devices/pci0000:00/0000:00:01.1/0000:c1:00.0/host0/port-0:9/expander-0:0/sas_device/expander-0:0/sas_address", "0x5\n")
	got, err := Discover(f.fsys)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	for _, h := range got {
		if want := h.Number == 0; h.HasExpander != want {
			t.Errorf("Discover() host%d HasExpander = %t, want %t", h.Number, h.HasExpander, want)
		}
	}
}

// TestCaptureRoundTrip checks that a capture made through capture.Recorder
// reproduces the device-mapper stack and the expander, which discovery
// learns only from directory listings.
func TestCaptureRoundTrip(t *testing.T) {
	f := pveFixture(t)
	f.addDM("dm-0", "mpatha", "sdm", "sdn")
	f.addDM("dm-1", "mpatha-part1", "dm-0")
	f.file("sys/devices/pci0000:00/0000:00:01.1/0000:c1:00.0/host0/port-0:9/expander-0:0/sas_device/expander-0:0/sas_address", "0x5\n")

	rec := capture.NewRecorder(f.fsys)
	wantHosts, err := Discover(rec)
	if err != nil {
		t.Fatalf("Discover(recorder) error: %v", err)
	}
	wantDisks, err := PhysicalDisks(rec, "/dev/mapper/mpatha-part1")
	if err != nil {
		t.Fatalf("PhysicalDisks(recorder) error: %v", err)
	}
	dir := t.TempDir()
	if err := rec.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}

	replay := os.DirFS(dir)
	gotHosts, err := Discover(replay)
	if err != nil {
		t.Fatalf("Discover(replay) error: %v", err)
	}
	if diff := cmp.Diff(wantHosts, gotHosts); diff != "" {
		t.Errorf("Discover(replay) mismatch (-recorded +replayed):\n%s", diff)
	}
	if !gotHosts[0].HasExpander {
		t.Error("Discover(replay) lost the expander")
	}
	gotDisks, err := PhysicalDisks(replay, "/dev/mapper/mpatha-part1")
	if err != nil {
		t.Fatalf("PhysicalDisks(replay) error: %v", err)
	}
	if diff := cmp.Diff(wantDisks, gotDisks); diff != "" {
		t.Errorf("PhysicalDisks(replay) mismatch (-recorded +replayed):\n%s", diff)
	}
}

func TestWholeDiskErrors(t *testing.T) {
	f := pveFixture(t)
	for _, devPath := range []string{
		"/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3", // Missing disk.
		"/dev/disk/by-id/wwn-0xloop",                                       // Link loop.
		"/sys/class/block/sda",                                             // Not under /dev.
	} {
		if got, err := WholeDisk(f.fsys, devPath); err == nil {
			t.Errorf("WholeDisk(%q) = %q, want error", devPath, got)
		}
	}
}

func TestResolve(t *testing.T) {
	f := newFakeSys(t)
	f.file("a/b/file", "")
	f.link("a/rel", "b/file")
	f.link("a/up", "../a/b")
	f.link("a/abs", "/a/b")
	f.link("a/chain", "rel")
	tests := []struct {
		name string
		want string
	}{
		{"a/rel", "a/b/file"},
		{"a/up/file", "a/b/file"},
		{"a/abs/file", "a/b/file"},
		{"a/chain", "a/b/file"},
		{"a/./b/../b/file", "a/b/file"},
	}
	for _, tc := range tests {
		got, err := Resolve(f.fsys, tc.name)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseUnitSerial(t *testing.T) {
	tests := []struct {
		name string
		vpd  []byte
		want string
	}{
		{"padded", []byte(vpdPage80("S460S1L7")), "S460S1L7"},
		{"truncated length", []byte{0, 0x80, 0, 20, 'A', 'B'}, "AB"},
		{"short header", []byte{0, 0x80}, ""},
		{"nul padded", []byte{0, 0x80, 0, 4, 'X', 'Y', 0, 0}, "XY"},
	}
	for _, tc := range tests {
		if got := parseUnitSerial(tc.vpd); got != tc.want {
			t.Errorf("parseUnitSerial(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
