package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaneshort/gdiskmap/internal/inventory"
	"github.com/shaneshort/gdiskmap/internal/layout"
	"github.com/shaneshort/gdiskmap/internal/mpt"
	"github.com/shaneshort/gdiskmap/internal/sysfs"
)

// writeCapture writes a capture directory, as `gdiskmap dump` produces, for
// one SAS2 controller with two disks in an eight-slot enclosure and a
// degraded pool whose second member is missing.
func writeCapture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	file := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name, target string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}

	host := "sys/devices/pci0000:00/0000:00:01.1/0000:01:00.0/host0"
	link("sys/class/scsi_host/host0", "../../devices/pci0000:00/0000:00:01.1/0000:01:00.0/host0/scsi_host/host0")
	file(host+"/scsi_host/host0/proc_name", "mpt2sas\n")
	file(host+"/scsi_host/host0/unique_id", "0\n")
	file(host+"/scsi_host/host0/version_fw", "20.00.07.00\n")
	file(host+"/scsi_host/host0/board_name", "SAS9207-8i\n")
	file(host+"/scsi_host/host0/host_sas_address", "0x500605b005ea4a10\n")
	for _, p := range []string{"phy-0:0", "phy-0:3"} {
		for _, c := range []string{"invalid_dword_count", "running_disparity_error_count", "loss_of_dword_sync_count", "phy_reset_problem_count"} {
			file(host+"/"+p+"/sas_phy/"+p+"/"+c, "0\n")
		}
	}
	disks := []struct{ port, phy, bay, block, serial, handle string }{
		{"0", "3", "0", "sdp", "PHYG006400DA240AGN", "0x0009"},
		{"1", "0", "3", "sdq", "S2PWNX0J607893A", "0x000a"},
	}
	for _, d := range disks {
		port := host + "/port-0:" + d.port
		end := port + "/end_device-0:" + d.port
		link(port+"/phy-0:"+d.phy, "../phy-0:"+d.phy)
		file(end+"/sas_device/end_device-0:"+d.port+"/sas_address", "0x443322110"+d.phy+"000000\n")
		file(end+"/sas_device/end_device-0:"+d.port+"/enclosure_identifier", "0x500605b005ea4a10\n")
		file(end+"/sas_device/end_device-0:"+d.port+"/bay_identifier", d.bay+"\n")
		lunRel := "target0:0:" + d.port + "/0:0:" + d.port + ":0"
		lun := end + "/" + lunRel
		file(lun+"/vendor", "ATA\n")
		file(lun+"/model", "SSD\n")
		file(lun+"/wwid", "naa.500000000000000"+d.port+"\n")
		file(lun+"/sas_device_handle", d.handle+"\n")
		file(lun+"/vpd_pg80", string([]byte{0, 0x80, 0, byte(len(d.serial))})+d.serial)
		file(lun+"/block/"+d.block+"/size", "468862128\n")
		file(lun+"/block/"+d.block+"/"+d.block+"3/partition", "3\n")
		rel := "../../devices/pci0000:00/0000:00:01.1/0000:01:00.0/host0/port-0:" + d.port + "/end_device-0:" + d.port + "/" + lunRel + "/block/" + d.block
		link("sys/class/block/"+d.block, rel)
		link("sys/class/block/"+d.block+"3", rel+"/"+d.block+"3")
		file("dev/"+d.block, "")
		file("dev/"+d.block+"3", "")
	}
	link("dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3", "../../sdp3")

	enclosures := map[int][]map[string]any{0: {{"handle": 1, "logicalId": 0x500605b005ea4a10, "startSlot": 0, "numSlots": 8}}}
	data, err := json.Marshal(enclosures)
	if err != nil {
		t.Fatal(err)
	}
	file(dumpEnclosuresFile, string(data))
	file(dumpZpoolFile, `{"pools": {"rpool": {"name": "rpool", "vdevs": {"rpool": {"name": "rpool", "vdevs": {"mirror-0": {"name": "mirror-0", "vdev_type": "mirror", "vdevs": {
		"ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3": {"name": "ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3", "vdev_type": "disk", "guid": "1", "path": "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3", "state": "ONLINE"},
		"5587783532978086300": {"name": "5587783532978086300", "vdev_type": "disk", "guid": "5587783532978086300", "path": "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3", "state": "UNAVAIL"}
	}}}}}}}}`)
	return dir
}

// runCLI runs gdiskmap with the given arguments and returns its exit status,
// stdout and stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestDisksFromCapture(t *testing.T) {
	dir := writeCapture(t)
	cfg := filepath.Join(t.TempDir(), "gdiskmap.toml")
	if err := os.WriteFile(cfg, []byte("[[enclosure]]\nid = \"0x500605b005ea4a10\"\n[enclosure.slots]\n0 = \"boot-a\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "-root", dir, "-config", cfg, "disks")
	if code != 0 {
		t.Fatalf("gdiskmap disks exit status = %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{
		"boot-a  c0:s0  sdp",
		"rpool  mirror-0  ONLINE",
		"c0:s1   c0:s1  -       (empty)",
		"c0:s3   c0:s3  sdq",
		"Pool members not found on any device:",
		"5587783532978086300  UNAVAIL  /dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("gdiskmap disks output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stderr, "warning") {
		t.Errorf("gdiskmap disks stderr has warnings:\n%s", stderr)
	}
}

func TestDisksJSONFromCapture(t *testing.T) {
	dir := writeCapture(t)
	code, stdout, stderr := runCLI(t, "-root", dir, "-config", filepath.Join(dir, "absent.toml"), "disks", "-json")
	if code != 0 {
		t.Fatalf("gdiskmap disks -json exit status = %d, stderr:\n%s", code, stderr)
	}
	var got struct {
		Slots []struct {
			Label string `json:"label"`
		} `json:"slots"`
		Missing []struct {
			Name string `json:"name"`
		} `json:"missing"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("gdiskmap disks -json output does not decode: %v\n%s", err, stdout)
	}
	if len(got.Slots) != 8 || len(got.Missing) != 1 {
		t.Errorf("gdiskmap disks -json: %d slots, %d missing; want 8 and 1", len(got.Slots), len(got.Missing))
	}
}

func TestCLIErrors(t *testing.T) {
	dir := writeCapture(t)
	cfg := filepath.Join(dir, "absent.toml")
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"unknown command", []string{"frobnicate"}, 2, "unknown command"},
		{"led without state", []string{"-root", dir, "locate", "sdp"}, 2, "usage: gdiskmap locate on|off"},
		{"led with -root", []string{"-root", dir, "-config", cfg, "locate", "on", "sdp"}, 1, "not available when replaying"},
		{"calibrate with -root", []string{"-root", dir, "-config", cfg, "calibrate"}, 1, "not available when replaying"},
		{"dump with -root", []string{"-root", dir, "dump", t.TempDir()}, 2, "cannot be combined with -root"},
		{"disks extra args", []string{"-root", dir, "disks", "x"}, 2, "unexpected arguments"},
		{"disks bad flag", []string{"-root", dir, "disks", "-bogus"}, 2, "flag provided but not defined"},
		{"no controllers", []string{"-root", t.TempDir(), "disks"}, 1, "SCSI hosts"},
	}
	for _, tc := range tests {
		code, _, stderr := runCLI(t, tc.args...)
		if code != tc.wantCode || !strings.Contains(stderr, tc.wantErr) {
			t.Errorf("%s: exit status %d, stderr %q; want %d and %q", tc.name, code, stderr, tc.wantCode, tc.wantErr)
		}
	}
}

func TestHelp(t *testing.T) {
	code, _, stderr := runCLI(t, "help")
	if code != 0 || !strings.Contains(stderr, "calibrate") {
		t.Errorf("gdiskmap help: exit status %d, output %q", code, stderr)
	}
}

func TestParseResponse(t *testing.T) {
	tests := []struct {
		line         string
		confirmation bool
		want         layout.Response
		wantOK       bool
	}{
		{"7", false, layout.Response{Kind: layout.RespBay, Bay: 7}, true},
		{"S", false, layout.Response{Kind: layout.RespSkip}, true},
		{"quit", false, layout.Response{Kind: layout.RespQuit}, true},
		{"", false, layout.Response{}, false},
		{"y", false, layout.Response{}, false},
		{"", true, layout.Response{Kind: layout.RespYes}, true},
		{"No", true, layout.Response{Kind: layout.RespNo}, true},
		{"12", true, layout.Response{Kind: layout.RespBay, Bay: 12}, true},
		{"maybe", true, layout.Response{}, false},
	}
	for _, tc := range tests {
		got, ok := parseResponse(tc.line, tc.confirmation)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseResponse(%q, %t) = %+v, %t; want %+v, %t", tc.line, tc.confirmation, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestCalibrationTargets(t *testing.T) {
	inv := &inventory.Inventory{Controllers: []inventory.Controller{
		{Name: "c0", PCIAddress: "0000:c1:00.0", Enclosures: []inventory.Enclosure{{ID: 0xa, StartSlot: 0, NumSlots: 2, FromFirmware: true}}},
		{Name: "c1", PCIAddress: "0000:01:00.0", Enclosures: []inventory.Enclosure{{ID: 0xb, StartSlot: 4, NumSlots: 2, FromFirmware: true}}},
		{Name: "c2", Enclosures: []inventory.Enclosure{{ID: 0xc, NumSlots: 1}}},
	}}
	got, err := calibrationTargets(inv, []string{"0000:01:00.0", "0xa"})
	if err != nil {
		t.Fatalf("calibrationTargets() error: %v", err)
	}
	if len(got) != 2 || got[0].enc.ID != 0xa || got[1].enc.ID != 0xb || len(got[1].slots) != 2 || got[1].slots[0] != 4 {
		t.Errorf("calibrationTargets() = %+v, want enclosures 0xa and 0xb with their slots", got)
	}
	if _, err := calibrationTargets(inv, []string{"c9"}); err == nil {
		t.Error("calibrationTargets(unknown) error = nil, want error")
	}
	if _, err := calibrationTargets(inv, []string{"c2"}); err == nil {
		t.Error("calibrationTargets(enclosure not read from firmware) error = nil, want error")
	}
}

func TestAddress(t *testing.T) {
	disk := &inventory.Disk{Device: sysfs.Device{Handle: 0x0e, HasHandle: true}}
	sys := &system{inv: &inventory.Inventory{Controllers: []inventory.Controller{
		{Enclosures: []inventory.Enclosure{{Handle: 1, FromFirmware: true}}},
		{Enclosures: []inventory.Enclosure{{Handle: 0, FromFirmware: false}}},
	}}}
	tests := []struct {
		name    string
		slot    inventory.Slot
		want    mpt.SlotAddress
		wantErr bool
	}{
		{"occupied, firmware enclosure", inventory.Slot{Controller: 0, Enclosure: 0, Number: 2, Disk: disk}, mpt.EnclosureSlotAddress(1, 2), false},
		{"empty, firmware enclosure", inventory.Slot{Controller: 0, Enclosure: 0, Number: 6}, mpt.EnclosureSlotAddress(1, 6), false},
		{"occupied, derived enclosure", inventory.Slot{Controller: 1, Enclosure: 0, Number: 2, Disk: disk}, mpt.DeviceAddress(0x0e), false},
		{"unplaced disk", inventory.Slot{Controller: 1, Enclosure: -1, Number: -1, Disk: disk}, mpt.DeviceAddress(0x0e), false},
		{"empty, derived enclosure", inventory.Slot{Controller: 1, Enclosure: 0, Number: 3}, mpt.SlotAddress{}, true},
	}
	for _, tc := range tests {
		got, err := sys.address(&tc.slot)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("address(%s) = %v, %v; want %v, error %t", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestUnusableCommand(t *testing.T) {
	dir := writeCapture(t)
	cfg := filepath.Join(t.TempDir(), "gdiskmap.toml")
	code, stdout, stderr := runCLI(t, "-root", dir, "-config", cfg, "unusable", "on", "-reason", "clip broken", "c0:s1", "c0:s2")
	if code != 0 {
		t.Fatalf("gdiskmap unusable on: exit status %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "c0:s1 (empty): unusable on") {
		t.Errorf("gdiskmap unusable on output = %q", stdout)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if !strings.Contains(string(data), `1 = "clip broken"`) {
		t.Errorf("config lacks the unusable mark:\n%s", data)
	}
	_, stdout, _ = runCLI(t, "-root", dir, "-config", cfg, "disks")
	if !strings.Contains(stdout, "(unusable)") {
		t.Errorf("gdiskmap disks does not show the unusable bay:\n%s", stdout)
	}

	if code, _, stderr := runCLI(t, "-root", dir, "-config", cfg, "unusable", "off", "c0:s1"); code != 0 {
		t.Fatalf("gdiskmap unusable off: exit status %d, stderr:\n%s", code, stderr)
	}
	_, stdout, _ = runCLI(t, "-root", dir, "-config", cfg, "disks")
	if strings.Count(stdout, "(unusable)") != 1 {
		t.Errorf("gdiskmap disks after unusable off: want one unusable bay:\n%s", stdout)
	}

	for _, args := range [][]string{
		{"unusable"},
		{"unusable", "on"},
		{"unusable", "maybe", "c0:s1"},
		{"unusable", "off", "-reason", "x", "c0:s1"},
	} {
		if code, _, _ := runCLI(t, append([]string{"-root", dir, "-config", cfg}, args...)...); code != 2 {
			t.Errorf("gdiskmap %q: exit status %d, want 2", args, code)
		}
	}
}
