// Package sysfs discovers LSI SAS HBAs driven by the mpt3sas driver, the
// devices attached to them and their per-phy link error counters, by reading
// the Linux sysfs tree.
//
// All reads go through an fs.FS rooted at "/" (paths such as
// "sys/class/scsi_host") so that discovery can run against a captured copy of
// the tree. The file system must implement fs.ReadLinkFS, because sysfs is
// navigated by resolving symbolic links.
package sysfs

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Driver names reported in /sys/class/scsi_host/hostN/proc_name by mpt3sas.
// The driver registers SAS2 controllers under the legacy mpt2sas name.
const (
	DriverSAS2 = "mpt2sas"
	DriverSAS3 = "mpt3sas"
)

// Host is a SCSI host driven by mpt3sas, i.e. one LSI SAS HBA.
type Host struct {
	// Number is the Linux SCSI host number (the N in hostN).
	Number int `json:"host"`
	// Driver is DriverSAS2 or DriverSAS3. It selects the ctl device used for
	// MPI requests.
	Driver string `json:"driver"`
	// IOC is the driver's adapter number (/sys/class/scsi_host/hostN/unique_id).
	// The ctl ioctl addresses controllers by this number. SAS2 and SAS3
	// controllers are numbered independently.
	IOC int `json:"ioc"`
	// PCIAddress is the controller's PCI address, e.g. "0000:01:00.0".
	PCIAddress string `json:"pciAddress"`
	// Firmware is the controller firmware version.
	Firmware string `json:"firmware"`
	// Board is the board name stored in the controller's manufacturing data.
	Board string `json:"board"`
	// SASAddress is the controller's SAS address. For direct-attach
	// backplanes it is also the logical ID of the controller's virtual
	// enclosure.
	SASAddress uint64 `json:"sasAddress"`
	// Phys are the controller's phys, in phy number order.
	Phys []Phy `json:"phys"`
	// Devices are the devices attached directly to the controller.
	Devices []Device `json:"devices"`
}

// Phy holds a controller phy's link error counters. Non-zero counts point at
// a bad cable, backplane slot or connector.
type Phy struct {
	Number           int    `json:"number"`
	InvalidDwords    uint64 `json:"invalidDwords"`
	DisparityErrors  uint64 `json:"disparityErrors"`
	LossOfDwordSync  uint64 `json:"lossOfDwordSync"`
	PhyResetProblems uint64 `json:"phyResetProblems"`
}

// Errors returns the sum of the phy's error counters.
func (p Phy) Errors() uint64 {
	return p.InvalidDwords + p.DisparityErrors + p.LossOfDwordSync + p.PhyResetProblems
}

// Device is a SCSI logical unit on an end device attached to a controller.
type Device struct {
	// EndDevice is the SAS transport name, e.g. "end_device-1:0".
	EndDevice string `json:"endDevice"`
	// SASAddress is the end device's SAS address. SATA disks receive a
	// synthetic address that repeats across controllers, so it is not a
	// unique key.
	SASAddress uint64 `json:"sasAddress"`
	// EnclosureID is the logical ID of the enclosure holding the device.
	EnclosureID uint64 `json:"enclosureId"`
	// Bay is the slot number reported by the controller firmware, or -1 if
	// unknown.
	Bay int `json:"bay"`
	// Phys are the controller phys the device is attached through.
	Phys []int `json:"phys"`
	// Handle is the firmware device handle; it is valid only if HasHandle is
	// set. MPI requests address the device by this handle.
	Handle    uint16 `json:"handle"`
	HasHandle bool   `json:"hasHandle"`
	// Block is the block device name (e.g. "sdb"), or empty if the logical
	// unit has none.
	Block  string `json:"block"`
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	// Serial is the unit serial number from VPD page 0x80.
	Serial string `json:"serial"`
	// WWID is the kernel's world wide identifier, e.g. "naa.5000c5009804f337".
	WWID string `json:"wwid"`
	// Size is the block device's capacity in bytes.
	Size uint64 `json:"size"`
}

var (
	hostDirPattern   = regexp.MustCompile(`^host\d+$`)
	pciAddrPattern   = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	portPattern      = regexp.MustCompile(`^port-\d+:\d+$`)
	phyPattern       = regexp.MustCompile(`^phy-\d+:(\d+)$`)
	endDevicePattern = regexp.MustCompile(`^end_device-\d+:\d+$`)
	targetPattern    = regexp.MustCompile(`^target\d+:\d+:\d+$`)
	lunPattern       = regexp.MustCompile(`^\d+:\d+:\d+:\d+$`)
)

// Discover returns every mpt2sas/mpt3sas host in fsys, ordered by host
// number. It returns an empty slice, not an error, if there are none.
func Discover(fsys fs.FS) ([]Host, error) {
	entries, err := fs.ReadDir(fsys, "sys/class/scsi_host")
	if err != nil {
		return nil, fmt.Errorf("listing SCSI hosts: %w", err)
	}
	var hosts []Host
	for _, e := range entries {
		if !hostDirPattern.MatchString(e.Name()) {
			continue
		}
		h, ok, err := readHost(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
		}
		if ok {
			hosts = append(hosts, h)
		}
	}
	slices.SortFunc(hosts, func(a, b Host) int { return a.Number - b.Number })
	return hosts, nil
}

// readHost reads one SCSI host. It reports ok=false for hosts not driven by
// mpt3sas.
func readHost(fsys fs.FS, name string) (h Host, ok bool, err error) {
	// The class entry resolves to
	// sys/devices/<pci path>/<pci addr>/hostN/scsi_host/hostN.
	classDir, err := Resolve(fsys, path.Join("sys/class/scsi_host", name))
	if err != nil {
		return Host{}, false, err
	}
	driver, err := readAttr(fsys, classDir, "proc_name")
	if err != nil {
		return Host{}, false, err
	}
	if driver != DriverSAS2 && driver != DriverSAS3 {
		return Host{}, false, nil
	}
	h = Host{Driver: driver}
	if h.Number, err = strconv.Atoi(strings.TrimPrefix(name, "host")); err != nil {
		return Host{}, false, fmt.Errorf("parsing host number: %w", err)
	}
	if h.IOC, err = readInt(fsys, classDir, "unique_id"); err != nil {
		return Host{}, false, err
	}
	if h.Firmware, err = readAttr(fsys, classDir, "version_fw"); err != nil {
		return Host{}, false, err
	}
	if h.Board, err = readAttr(fsys, classDir, "board_name"); err != nil {
		return Host{}, false, err
	}
	if h.SASAddress, err = readHex(fsys, classDir, "host_sas_address"); err != nil {
		return Host{}, false, err
	}

	hostDir := path.Dir(path.Dir(classDir))
	if pci := path.Base(path.Dir(hostDir)); pciAddrPattern.MatchString(pci) {
		h.PCIAddress = pci
	}
	if h.Phys, err = readPhys(fsys, hostDir); err != nil {
		return Host{}, false, err
	}
	if h.Devices, err = readPorts(fsys, hostDir); err != nil {
		return Host{}, false, err
	}
	return h, true, nil
}

// readPhys reads the controller phys under hostDir.
func readPhys(fsys fs.FS, hostDir string) ([]Phy, error) {
	entries, err := fs.ReadDir(fsys, hostDir)
	if err != nil {
		return nil, err
	}
	var phys []Phy
	for _, e := range entries {
		m := phyPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1]) // The pattern guarantees digits.
		dir := path.Join(hostDir, e.Name(), "sas_phy", e.Name())
		p := Phy{Number: n}
		counters := []struct {
			attr string
			dst  *uint64
		}{
			{"invalid_dword_count", &p.InvalidDwords},
			{"running_disparity_error_count", &p.DisparityErrors},
			{"loss_of_dword_sync_count", &p.LossOfDwordSync},
			{"phy_reset_problem_count", &p.PhyResetProblems},
		}
		for _, c := range counters {
			v, err := readUint(fsys, dir, c.attr)
			if err != nil {
				return nil, err
			}
			*c.dst = v
		}
		phys = append(phys, p)
	}
	slices.SortFunc(phys, func(a, b Phy) int { return a.Number - b.Number })
	return phys, nil
}

// readPorts reads the devices attached to each SAS port under hostDir.
// Expanders are not descended into; expander-attached enclosures are outside
// gdiskmap's scope.
func readPorts(fsys fs.FS, hostDir string) ([]Device, error) {
	ports, err := fs.ReadDir(fsys, hostDir)
	if err != nil {
		return nil, err
	}
	var devices []Device
	for _, p := range ports {
		if !portPattern.MatchString(p.Name()) {
			continue
		}
		portDir := path.Join(hostDir, p.Name())
		entries, err := fs.ReadDir(fsys, portDir)
		if err != nil {
			return nil, err
		}
		var phys []int
		var ends []string
		for _, e := range entries {
			if m := phyPattern.FindStringSubmatch(e.Name()); m != nil {
				n, _ := strconv.Atoi(m[1]) // The pattern guarantees digits.
				phys = append(phys, n)
			} else if endDevicePattern.MatchString(e.Name()) {
				ends = append(ends, e.Name())
			}
		}
		slices.Sort(phys)
		for _, end := range ends {
			ds, err := readEndDevice(fsys, path.Join(portDir, end), phys)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", end, err)
			}
			devices = append(devices, ds...)
		}
	}
	return devices, nil
}

// readEndDevice reads the logical units of the end device at dir.
func readEndDevice(fsys fs.FS, dir string, phys []int) ([]Device, error) {
	name := path.Base(dir)
	attrs := path.Join(dir, "sas_device", name)
	base := Device{EndDevice: name, Phys: phys, Bay: -1}
	var err error
	if base.SASAddress, err = readHex(fsys, attrs, "sas_address"); err != nil {
		return nil, err
	}
	// The driver returns an error rather than a value when it cannot look the
	// device up; that leaves the enclosure and bay unknown rather than
	// failing discovery.
	if id, err := readHex(fsys, attrs, "enclosure_identifier"); err == nil {
		base.EnclosureID = id
	}
	if bay, err := readInt(fsys, attrs, "bay_identifier"); err == nil && bay >= 0 {
		base.Bay = bay
	}

	targets, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var devices []Device
	for _, t := range targets {
		if !targetPattern.MatchString(t.Name()) {
			continue
		}
		targetDir := path.Join(dir, t.Name())
		luns, err := fs.ReadDir(fsys, targetDir)
		if err != nil {
			return nil, err
		}
		for _, l := range luns {
			if !lunPattern.MatchString(l.Name()) {
				continue
			}
			d, err := readLUN(fsys, path.Join(targetDir, l.Name()), base)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", l.Name(), err)
			}
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 {
		// An end device without a logical unit still occupies its slot.
		devices = append(devices, base)
	}
	return devices, nil
}

// readLUN fills in the SCSI device attributes of the logical unit at dir.
func readLUN(fsys fs.FS, dir string, d Device) (Device, error) {
	var err error
	if d.Vendor, err = readAttr(fsys, dir, "vendor"); err != nil {
		return Device{}, err
	}
	if d.Model, err = readAttr(fsys, dir, "model"); err != nil {
		return Device{}, err
	}
	if d.WWID, err = readAttr(fsys, dir, "wwid"); err != nil {
		return Device{}, err
	}
	switch handle, err := readHex(fsys, dir, "sas_device_handle"); {
	case err == nil:
		d.Handle, d.HasHandle = uint16(handle), true
	case !errors.Is(err, fs.ErrNotExist):
		return Device{}, err
	}
	switch vpd, err := fs.ReadFile(fsys, path.Join(dir, "vpd_pg80")); {
	case err == nil:
		d.Serial = parseUnitSerial(vpd)
	case !errors.Is(err, fs.ErrNotExist):
		return Device{}, err
	}

	blocks, err := fs.ReadDir(fsys, path.Join(dir, "block"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return d, nil
	case err != nil:
		return Device{}, err
	case len(blocks) > 0:
		d.Block = blocks[0].Name()
		sectors, err := readUint(fsys, path.Join(dir, "block", d.Block), "size")
		if err != nil {
			return Device{}, err
		}
		// The block layer reports size in 512-byte units regardless of the
		// device's logical block size.
		d.Size = sectors * 512
	}
	return d, nil
}

// parseUnitSerial extracts the serial number from a VPD page 0x80 (Unit
// Serial Number) response: a 4-byte header whose bytes 2-3 hold the page
// length, followed by the ASCII serial number.
func parseUnitSerial(vpd []byte) string {
	if len(vpd) < 4 {
		return ""
	}
	n := int(vpd[2])<<8 | int(vpd[3])
	end := min(4+n, len(vpd))
	return strings.TrimSpace(strings.TrimRight(string(vpd[4:end]), "\x00"))
}

// WholeDisk returns the name of the whole-disk block device that devPath
// (e.g. "/dev/disk/by-id/ata-...-part3" or "/dev/sdb") refers to, resolving
// symbolic links and mapping partitions to their parent disk.
func WholeDisk(fsys fs.FS, devPath string) (string, error) {
	dev, err := Resolve(fsys, strings.TrimPrefix(devPath, "/"))
	if err != nil {
		return "", err
	}
	if path.Dir(dev) != "dev" {
		return "", fmt.Errorf("%s does not refer to a device node", devPath)
	}
	// sys/class/block/<name> resolves to .../block/<disk> for a disk and to
	// .../block/<disk>/<partition> for a partition.
	sysDir, err := Resolve(fsys, path.Join("sys/class/block", path.Base(dev)))
	if err != nil {
		return "", err
	}
	parent := path.Dir(sysDir)
	if path.Base(path.Dir(parent)) == "block" {
		return path.Base(parent), nil
	}
	return path.Base(sysDir), nil
}

// Resolve returns name with every symbolic link in it resolved, in the manner
// of realpath(3), for a file system rooted at "/". Absolute link targets are
// interpreted relative to the root of fsys.
func Resolve(fsys fs.FS, name string) (string, error) {
	// maxLinks matches the Linux limit on links followed in one lookup.
	const maxLinks = 40
	pending := strings.Split(name, "/")
	var resolved []string
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
			continue
		}
		cur := path.Join(path.Join(resolved...), part)
		info, err := fs.Lstat(fsys, cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			resolved = append(resolved, part)
			continue
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("resolving %s: too many levels of symbolic links", name)
		}
		target, err := fs.ReadLink(fsys, cur)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(target, "/") {
			resolved = nil
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	return path.Join(resolved...), nil
}

// readAttr returns the whitespace-trimmed contents of the sysfs attribute
// dir/name, or "" if the attribute does not exist.
func readAttr(fsys fs.FS, dir, name string) (string, error) {
	b, err := fs.ReadFile(fsys, path.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// readRequired is readAttr for attributes that must exist.
func readRequired(fsys fs.FS, dir, name string) (string, error) {
	b, err := fs.ReadFile(fsys, path.Join(dir, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func readInt(fsys fs.FS, dir, name string) (int, error) {
	s, err := readRequired(fsys, dir, name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path.Join(dir, name), err)
	}
	return v, nil
}

func readUint(fsys fs.FS, dir, name string) (uint64, error) {
	s, err := readRequired(fsys, dir, name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path.Join(dir, name), err)
	}
	return v, nil
}

// readHex parses a hexadecimal attribute such as "0x500605b0013ca580".
func readHex(fsys fs.FS, dir, name string) (uint64, error) {
	s, err := readRequired(fsys, dir, name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path.Join(dir, name), err)
	}
	return v, nil
}
