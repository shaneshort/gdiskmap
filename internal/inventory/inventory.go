// Package inventory merges controller discovery, firmware enclosure data,
// ZFS pool membership and the layout config into a list of slots, and
// resolves user-supplied targets to slots.
package inventory

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"gdiskmap/internal/layout"
	"gdiskmap/internal/mpt"
	"gdiskmap/internal/sysfs"
	"gdiskmap/internal/zfs"
)

// Inventory is the merged view of every controller slot.
type Inventory struct {
	Controllers []Controller `json:"controllers"`
	// Slots lists every known slot, ordered by controller, enclosure and
	// slot number. A slot the firmware reports more than one device in
	// appears once per device.
	Slots []Slot `json:"slots"`
	// Unplaced lists devices whose slot is unknown.
	Unplaced []Slot `json:"unplaced"`
	// PoolsKnown reports whether pool membership was determined. If not,
	// a disk without vdevs may still be in a pool.
	PoolsKnown bool `json:"poolsKnown"`
	// Missing lists pool members whose device does not exist.
	Missing []zfs.Vdev `json:"missing"`
	// Warnings describes problems found while merging.
	Warnings []string `json:"warnings"`
}

// Controller is one HBA.
type Controller struct {
	// Name is the short name used in default slot labels, e.g. "c0".
	Name       string      `json:"name"`
	Driver     string      `json:"driver"`
	IOC        int         `json:"ioc"`
	Host       int         `json:"host"`
	PCIAddress string      `json:"pciAddress"`
	Firmware   string      `json:"firmware"`
	Board      string      `json:"board"`
	SASAddress uint64      `json:"sasAddress"`
	Enclosures []Enclosure `json:"enclosures"`
}

// Enclosure is an enclosure attached to a controller.
type Enclosure struct {
	ID uint64 `json:"id"`
	// Handle is the firmware enclosure handle. It is known only if the
	// enclosure list was read from the firmware (FromFirmware).
	Handle       uint16 `json:"handle"`
	StartSlot    int    `json:"startSlot"`
	NumSlots     int    `json:"numSlots"`
	FromFirmware bool   `json:"fromFirmware"`
}

// Slot is one slot of an enclosure, with the disk in it if there is one.
type Slot struct {
	// Label is the configured bay label, or DefaultLabel.
	Label string `json:"label"`
	// DefaultLabel identifies the slot by controller, enclosure and slot
	// number, e.g. "c0:s3". It is always accepted as a target.
	DefaultLabel string `json:"defaultLabel"`
	// Controller indexes Inventory.Controllers.
	Controller int `json:"controller"`
	// Enclosure indexes the controller's Enclosures, or is -1 for an
	// unplaced device.
	Enclosure int `json:"enclosure"`
	// Number is the slot number, or -1 for an unplaced device.
	Number int   `json:"number"`
	Disk   *Disk `json:"disk,omitempty"`
	// LED is the slot's indicator state, if it was read.
	LED *LEDState `json:"led,omitempty"`
}

// Disk is a device in a slot.
type Disk struct {
	sysfs.Device
	// LinkErrors is the sum of the error counters of the controller phys
	// the device is attached through.
	LinkErrors uint64 `json:"linkErrors"`
	// Vdevs lists the pool vdevs on the disk (more than one if several of
	// its partitions are in use). An empty list means the disk is in no
	// pool.
	Vdevs []zfs.Vdev `json:"vdevs"`
}

// LEDState is the indicator state of a slot.
type LEDState struct {
	Identify bool   `json:"identify"`
	Fault    bool   `json:"fault"`
	Raw      uint32 `json:"raw"`
}

// NewLEDState decodes a SEP slot status word.
func NewLEDState(s mpt.SlotStatus) *LEDState {
	return &LEDState{
		Identify: s.Has(mpt.StatusIdentify),
		Fault:    s.Has(mpt.StatusPredictedFault),
		Raw:      uint32(s),
	}
}

// Sources holds the inputs to Build.
type Sources struct {
	Hosts []sysfs.Host
	// Enclosures holds the firmware enclosure list of each host, keyed by
	// host number. A host without an entry had its list derived from the
	// devices attached to it, which cannot reveal empty slots.
	Enclosures map[int][]mpt.Enclosure
	// Vdevs lists the leaf vdevs of the imported pools, if known.
	Vdevs []zfs.Vdev
	// WholeDisk resolves a vdev path to a whole-disk block device name. If
	// nil, pool membership is not determined.
	WholeDisk func(devPath string) (string, error)
	// Layout supplies bay labels. It may be nil.
	Layout *layout.Config
}

// Build merges src into an Inventory.
func Build(src Sources) *Inventory {
	inv := &Inventory{}
	hosts := slices.Clone(src.Hosts)
	// Order controllers as the vendor tools do: by IOC number, SAS2
	// controllers before SAS3, whose IOC numbers are independent.
	slices.SortFunc(hosts, func(a, b sysfs.Host) int {
		return cmp.Or(cmp.Compare(a.Driver, b.Driver), cmp.Compare(a.IOC, b.IOC))
	})
	for ci, h := range hosts {
		c := Controller{
			Name:       fmt.Sprintf("c%d", ci),
			Driver:     h.Driver,
			IOC:        h.IOC,
			Host:       h.Number,
			PCIAddress: h.PCIAddress,
			Firmware:   h.Firmware,
			Board:      h.Board,
			SASAddress: h.SASAddress,
		}
		fw, ok := src.Enclosures[h.Number]
		if ok {
			for _, e := range fw {
				if e.NumSlots == 0 {
					continue
				}
				c.Enclosures = append(c.Enclosures, Enclosure{
					ID: e.LogicalID, Handle: e.Handle,
					StartSlot: int(e.StartSlot), NumSlots: int(e.NumSlots), FromFirmware: true,
				})
			}
		} else {
			c.Enclosures = derivedEnclosures(h.Devices)
		}
		inv.Controllers = append(inv.Controllers, c)
		inv.placeDevices(ci, h, src.Layout)
	}
	if src.WholeDisk != nil {
		inv.PoolsKnown = true
		inv.addPoolMembership(src.Vdevs, src.WholeDisk)
	}
	return inv
}

// derivedEnclosures builds an enclosure list from the devices' enclosure IDs
// and bays, for use when the firmware list cannot be read. Only occupied
// slots are known.
func derivedEnclosures(devices []sysfs.Device) []Enclosure {
	var encls []Enclosure
	for _, d := range devices {
		if d.Bay < 0 {
			continue
		}
		i := slices.IndexFunc(encls, func(e Enclosure) bool { return e.ID == d.EnclosureID })
		if i < 0 {
			encls = append(encls, Enclosure{ID: d.EnclosureID, StartSlot: d.Bay, NumSlots: 1})
			continue
		}
		e := &encls[i]
		end := max(e.StartSlot+e.NumSlots, d.Bay+1)
		e.StartSlot = min(e.StartSlot, d.Bay)
		e.NumSlots = end - e.StartSlot
	}
	return encls
}

// placeDevices adds a slot for every slot of controller ci's enclosures and
// assigns h's devices to them.
func (inv *Inventory) placeDevices(ci int, h sysfs.Host, cfg *layout.Config) {
	c := &inv.Controllers[ci]
	placed := make([]bool, len(h.Devices))
	for ei, e := range c.Enclosures {
		for n := e.StartSlot; n < e.StartSlot+e.NumSlots; n++ {
			slot := Slot{Controller: ci, Enclosure: ei, Number: n}
			slot.DefaultLabel = defaultLabel(c, ei, n)
			slot.Label = slot.DefaultLabel
			if l, ok := cfg.Label(e.ID, n); ok {
				slot.Label = l
			}
			var names []string
			for di, d := range h.Devices {
				if d.EnclosureID != e.ID || d.Bay != n {
					continue
				}
				placed[di] = true
				s := slot
				s.Disk = newDisk(h, d)
				inv.Slots = append(inv.Slots, s)
				names = append(names, cmp.Or(d.Block, d.EndDevice))
			}
			switch {
			case len(names) == 0:
				if e.FromFirmware {
					inv.Slots = append(inv.Slots, slot)
				}
			case len(names) > 1:
				inv.Warnings = append(inv.Warnings, fmt.Sprintf(
					"%s: the controller reports %d devices in this slot (%s); its slot table is probably wrong, so slot numbers and LEDs for these disks are unreliable",
					slot.DefaultLabel, len(names), strings.Join(names, ", ")))
			}
		}
	}
	for di, d := range h.Devices {
		if placed[di] {
			continue
		}
		name := cmp.Or(d.Block, d.EndDevice)
		inv.Unplaced = append(inv.Unplaced, Slot{
			Label:        fmt.Sprintf("%s:%s", c.Name, name),
			DefaultLabel: fmt.Sprintf("%s:%s", c.Name, name),
			Controller:   ci,
			Enclosure:    -1,
			Number:       -1,
			Disk:         newDisk(h, d),
		})
		inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %s has no known slot (enclosure %#x, bay %d)", c.Name, name, d.EnclosureID, d.Bay))
	}
}

func defaultLabel(c *Controller, enclosure, slot int) string {
	if len(c.Enclosures) > 1 {
		return fmt.Sprintf("%s:e%d:s%d", c.Name, enclosure, slot)
	}
	return fmt.Sprintf("%s:s%d", c.Name, slot)
}

func newDisk(h sysfs.Host, d sysfs.Device) *Disk {
	disk := &Disk{Device: d}
	for _, p := range h.Phys {
		if slices.Contains(d.Phys, p.Number) {
			disk.LinkErrors += p.Errors()
		}
	}
	return disk
}

// addPoolMembership attaches each vdev to the disk holding it. Vdevs whose
// path no longer resolves to a device are listed as missing; vdevs on
// devices not attached to an HBA are ignored.
func (inv *Inventory) addPoolMembership(vdevs []zfs.Vdev, wholeDisk func(string) (string, error)) {
	byBlock := make(map[string]*Disk)
	for _, list := range [][]Slot{inv.Slots, inv.Unplaced} {
		for _, s := range list {
			if s.Disk != nil && s.Disk.Block != "" {
				byBlock[s.Disk.Block] = s.Disk
			}
		}
	}
	for _, v := range vdevs {
		if v.Path == "" {
			continue
		}
		block, err := wholeDisk(v.Path)
		if err != nil {
			inv.Missing = append(inv.Missing, v)
			continue
		}
		if d, ok := byBlock[block]; ok {
			d.Vdevs = append(d.Vdevs, v)
		}
	}
}
