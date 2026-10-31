package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shaneshort/gdiskmap/internal/inventory"
	"github.com/shaneshort/gdiskmap/internal/layout"
	"github.com/shaneshort/gdiskmap/internal/mpt"
	"github.com/shaneshort/gdiskmap/internal/sysfs"
	"github.com/shaneshort/gdiskmap/internal/zfs"
)

// Files a dump directory holds besides the captured sysfs and /dev entries.
const (
	dumpZpoolFile      = "zpool-status.json"
	dumpEnclosuresFile = "enclosures.json"
)

// zpoolTimeout bounds `zpool status`, which can hang on a pool with
// unresponsive devices.
const zpoolTimeout = 30 * time.Second

// env holds the command-line settings and I/O streams shared by commands.
type env struct {
	stdin      *bufio.Reader
	stdout     io.Writer
	stderr     io.Writer
	configPath string
	root       string
	zpool      string

	notes map[string]bool
}

// notef prints a note to stderr, once per distinct message.
func (e *env) notef(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.notes[msg] {
		return
	}
	if e.notes == nil {
		e.notes = make(map[string]bool)
	}
	e.notes[msg] = true
	fmt.Fprintf(e.stderr, "gdiskmap: %s\n", msg)
}

// liveFS returns the file system to read sysfs and /dev from: the root of
// the live system, or the -root capture directory.
func (e *env) liveFS() fs.ReadLinkFS {
	dir := "/"
	if e.root != "" {
		dir = e.root
	}
	// os.DirFS implements fs.ReadLinkFS since Go 1.25.
	return os.DirFS(dir).(fs.ReadLinkFS)
}

// system is the gathered state of the machine.
type system struct {
	fsys        fs.ReadLinkFS
	inv         *inventory.Inventory
	cfg         *layout.Config
	controllers map[int]*mpt.Controller // Open controllers, by host number.
	enclosures  map[int][]mpt.Enclosure // Firmware enclosure lists, by host number.
	zpoolJSON   []byte                  // Raw `zpool status -j` output, if read.
	unlockLEDs  func()                  // Releases the LED lock, if held.
}

// gatherOptions selects the optional parts of gathering.
type gatherOptions struct {
	// needControllers makes failure to open any controller an error, and
	// takes the LED lock (see lockLEDs) for the life of the system, before
	// anything is read.
	needControllers bool
	// readLEDs reads the LED state of every slot.
	readLEDs bool
}

// gather discovers controllers and disks in fsys and builds the inventory.
// Missing optional data (controller access, ZFS) is reported as a note.
func gather(e *env, fsys fs.ReadLinkFS, opts gatherOptions) (*system, error) {
	sys := &system{
		fsys:        fsys,
		controllers: make(map[int]*mpt.Controller),
		enclosures:  make(map[int][]mpt.Enclosure),
	}
	if opts.needControllers && e.root == "" {
		// The lock is taken before discovery and before the config is
		// loaded: a command that waited for a calibration to finish must
		// see the mapping that calibration saved, not the one before it.
		unlock, err := lockLEDs(e)
		if err != nil {
			return nil, err
		}
		sys.unlockLEDs = unlock
	}
	// fail releases the lock and any open controllers.
	fail := func(err error) (*system, error) {
		sys.close()
		return nil, err
	}
	hosts, err := sysfs.Discover(fsys)
	if err != nil {
		return fail(err)
	}
	if len(hosts) == 0 {
		return fail(errors.New("no LSI SAS controllers (mpt2sas/mpt3sas driver) found"))
	}
	if sys.cfg, err = layout.Load(e.configPath); err != nil {
		return fail(err)
	}
	if err := sys.openControllers(e, hosts, opts.needControllers); err != nil {
		return fail(err)
	}
	vdevs, poolsKnown := sys.readPools(e)

	src := inventory.Sources{
		Hosts:      hosts,
		Enclosures: sys.enclosures,
		Layout:     sys.cfg,
	}
	if poolsKnown {
		src.Vdevs = vdevs
		src.PhysicalDisks = sys.physicalDisks
	}
	sys.inv = inventory.Build(src)
	if opts.readLEDs {
		sys.readLEDs(e)
	}
	return sys, nil
}

// openControllers opens each controller's ctl device and reads its
// enclosure list. With -root, the enclosure lists come from the capture and
// no controller is opened.
func (sys *system) openControllers(e *env, hosts []sysfs.Host, required bool) error {
	if e.root != "" {
		if required {
			return errors.New("LED control is not available when replaying a capture with -root")
		}
		data, err := os.ReadFile(filepath.Join(e.root, dumpEnclosuresFile))
		if errors.Is(err, fs.ErrNotExist) {
			e.notef("the capture has no %s; empty slots are not shown", dumpEnclosuresFile)
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &sys.enclosures); err != nil {
			return fmt.Errorf("parsing %s: %w", dumpEnclosuresFile, err)
		}
		return nil
	}

	for _, h := range hosts {
		c, err := mpt.Open(h.Driver, h.IOC)
		if err != nil {
			if required {
				return fmt.Errorf("opening controller (host%d): %w", h.Number, err)
			}
			if errors.Is(err, fs.ErrPermission) {
				e.notef("%v; run as root to show empty slots and LED state", err)
			} else {
				e.notef("%v; empty slots and LED state are not shown", err)
			}
			continue
		}
		sys.controllers[h.Number] = c
		encls, err := c.Enclosures()
		if err != nil {
			e.notef("host%d: %v; its empty slots are not shown", h.Number, err)
			continue
		}
		sys.enclosures[h.Number] = encls
	}
	return nil
}

// readPools reads pool membership. It reports false if it is unavailable.
func (sys *system) readPools(e *env) ([]zfs.Vdev, bool) {
	var data []byte
	if e.root != "" {
		var err error
		data, err = os.ReadFile(filepath.Join(e.root, dumpZpoolFile))
		if err != nil {
			e.notef("pool membership is not shown: %v", err)
			return nil, false
		}
	} else {
		path, err := exec.LookPath(e.zpool)
		if err != nil {
			e.notef("pool membership is not shown: %v", err)
			return nil, false
		}
		ctx, cancel := context.WithTimeout(context.Background(), zpoolTimeout)
		defer cancel()
		data, err = zfs.Run(ctx, path)
		if err != nil {
			e.notef("pool membership is not shown: %v", err)
			return nil, false
		}
	}
	vdevs, err := zfs.Parse(data)
	if err != nil {
		e.notef("pool membership is not shown: %v", err)
		return nil, false
	}
	sys.zpoolJSON = data
	return vdevs, true
}

// readLEDs fills in the LED state of every slot on an open controller. Some
// firmware (e.g. SAS2 IT firmware driving SGPIO backplanes) rejects SEP
// read-status requests, so failures are summarised per controller rather
// than reported per slot.
func (sys *system) readLEDs(e *env) {
	type tally struct {
		ok, failed int
		lastErr    error
	}
	tallies := make([]tally, len(sys.inv.Controllers))
	for _, s := range sys.allSlots() {
		c := sys.controllers[sys.inv.Controllers[s.Controller].Host]
		if c == nil {
			continue
		}
		addr, err := sys.address(s)
		if err != nil {
			continue
		}
		t := &tallies[s.Controller]
		status, err := c.SlotStatus(addr)
		if err != nil {
			t.failed++
			t.lastErr = err
			continue
		}
		t.ok++
		s.LED = inventory.NewLEDState(status)
	}
	for i, t := range tallies {
		name := sys.inv.Controllers[i].Name
		switch {
		case t.failed == 0:
		case t.ok == 0:
			e.notef("%s: the controller does not report LED state (%v)", name, t.lastErr)
		default:
			e.notef("%s: LED state unavailable for %d of %d slots (%v)", name, t.failed, t.ok+t.failed, t.lastErr)
		}
	}
}

// allSlots returns pointers to every slot, placed or not.
func (sys *system) allSlots() []*inventory.Slot {
	var out []*inventory.Slot
	for i := range sys.inv.Slots {
		out = append(out, &sys.inv.Slots[i])
	}
	for i := range sys.inv.Unplaced {
		out = append(out, &sys.inv.Unplaced[i])
	}
	return out
}

// address returns the SEP address of a slot. Enclosure/slot addressing is
// preferred: SAS2 IT firmware driving an SGPIO backplane rejects read-status
// requests addressed by device handle and accepts, but ignores, write-status
// requests addressed that way (sas2ircu also uses enclosure/slot). The
// device handle is used only when the enclosure handle is unknown, i.e. the
// enclosure list could not be read from the firmware; on such firmware the
// status read then fails and mpt.Controller.SetIndicator writes nothing.
//
// A slot the controller places more than one device in has no address: its
// slot table is wrong, so its LED could mark a bay other than the disk's.
func (sys *system) address(s *inventory.Slot) (mpt.SlotAddress, error) {
	if s.Conflict {
		return mpt.SlotAddress{}, fmt.Errorf("%s: the controller reports more than one device in this slot, so its LED could mark the wrong bay; its slot table (NVDATA) needs fixing first", s.DefaultLabel)
	}
	if s.Enclosure >= 0 && s.Number >= 0 {
		enc := sys.inv.Controllers[s.Controller].Enclosures[s.Enclosure]
		if enc.FromFirmware {
			return mpt.EnclosureSlotAddress(enc.Handle, uint16(s.Number)), nil
		}
	}
	if s.Disk != nil && s.Disk.HasHandle {
		return mpt.DeviceAddress(s.Disk.Handle), nil
	}
	return mpt.SlotAddress{}, fmt.Errorf("%s: no enclosure handle or device handle to address the slot by", s.Label)
}

// controller returns the open controller holding a slot.
func (sys *system) controller(s *inventory.Slot) (*mpt.Controller, error) {
	c := sys.inv.Controllers[s.Controller]
	if mc := sys.controllers[c.Host]; mc != nil {
		return mc, nil
	}
	return nil, fmt.Errorf("controller %s (host%d) is not open", c.Name, c.Host)
}

func (sys *system) wholeDisk(devPath string) (string, error) {
	return sysfs.WholeDisk(sys.fsys, devPath)
}

func (sys *system) physicalDisks(devPath string) ([]string, error) {
	return sysfs.PhysicalDisks(sys.fsys, devPath)
}

// close releases the open controllers and the LED lock.
func (sys *system) close() {
	for _, c := range sys.controllers {
		c.Close()
	}
	if sys.unlockLEDs != nil {
		sys.unlockLEDs()
	}
}

// printWarnings prints the inventory's warnings to stderr.
func (sys *system) printWarnings(e *env) {
	for _, w := range sys.inv.Warnings {
		fmt.Fprintf(e.stderr, "gdiskmap: warning: %s\n", w)
	}
}
