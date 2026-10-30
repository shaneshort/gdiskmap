package inventory

import (
	"cmp"
	"fmt"
	"strings"
)

// Target keywords accepted by Select.
const (
	TargetAll      = "all"
	TargetOccupied = "occupied"
	TargetEmpty    = "empty"
)

// Select resolves targets to slots, in the order given and without
// duplicates. A target is one of:
//
//   - "all" (every slot, and every disk without a known slot), "occupied"
//     or "empty";
//   - a slot label or default label ("c0:s3");
//   - a block device name ("sdb") or device path ("/dev/sdb",
//     "/dev/disk/by-id/...");
//   - a serial number (case-insensitive);
//   - a WWID, with or without its "naa." or "0x" prefix.
//
// wholeDisk resolves device paths other than /dev/<name>; it may be nil.
// A target matching more than one slot (other than a keyword) is rejected as
// ambiguous.
func (inv *Inventory) Select(targets []string, wholeDisk func(devPath string) (string, error)) ([]*Slot, error) {
	var out []*Slot
	seen := make(map[*Slot]bool)
	add := func(s *Slot) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, t := range targets {
		matches, err := inv.match(t, wholeDisk)
		if err != nil {
			return nil, err
		}
		for _, s := range matches {
			add(s)
		}
	}
	return out, nil
}

// match returns the slots a single target refers to.
func (inv *Inventory) match(target string, wholeDisk func(string) (string, error)) ([]*Slot, error) {
	all := inv.allSlots()
	switch target {
	case TargetAll:
		return all, nil
	case TargetOccupied:
		return filter(all, func(s *Slot) bool { return s.Disk != nil }), nil
	case TargetEmpty:
		return filter(inv.slotPointers(), func(s *Slot) bool { return s.Disk == nil }), nil
	}

	block := ""
	if name, ok := strings.CutPrefix(target, "/dev/"); ok {
		block = name
		if strings.Contains(name, "/") {
			if wholeDisk == nil {
				return nil, fmt.Errorf("cannot resolve %s", target)
			}
			b, err := wholeDisk(target)
			if err != nil {
				return nil, fmt.Errorf("resolving %s: %w", target, err)
			}
			block = b
		}
	}

	matchers := []func(*Slot) bool{
		func(s *Slot) bool { return s.Label == target || s.DefaultLabel == target },
		func(s *Slot) bool {
			return s.Disk != nil && s.Disk.Block != "" && s.Disk.Block == cmp.Or(block, target)
		},
		func(s *Slot) bool {
			return s.Disk != nil && s.Disk.Serial != "" && strings.EqualFold(s.Disk.Serial, target)
		},
		func(s *Slot) bool { return s.Disk != nil && wwidMatches(s.Disk.WWID, target) },
	}
	// The first kind of identifier that matches anything decides the
	// result, so a label is never also interpreted as a serial number.
	for _, m := range matchers {
		found := filter(all, m)
		switch len(found) {
		case 0:
			continue
		case 1:
			return found, nil
		default:
			labels := make([]string, len(found))
			for i, s := range found {
				labels[i] = describe(s)
			}
			return nil, fmt.Errorf("%q is ambiguous; it matches %s", target, strings.Join(labels, ", "))
		}
	}
	return nil, fmt.Errorf("no slot or disk matches %q", target)
}

// allSlots returns pointers to every slot, placed or not.
func (inv *Inventory) allSlots() []*Slot {
	ptrs := inv.slotPointers()
	for i := range inv.Unplaced {
		ptrs = append(ptrs, &inv.Unplaced[i])
	}
	return ptrs
}

func (inv *Inventory) slotPointers() []*Slot {
	ptrs := make([]*Slot, len(inv.Slots))
	for i := range inv.Slots {
		ptrs[i] = &inv.Slots[i]
	}
	return ptrs
}

func filter(slots []*Slot, keep func(*Slot) bool) []*Slot {
	var out []*Slot
	for _, s := range slots {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// wwidMatches reports whether target identifies the device with the given
// WWID, e.g. "naa.5000c5009804f337", "5000c5009804f337" or
// "0x5000c5009804f337".
func wwidMatches(wwid, target string) bool {
	if wwid == "" {
		return false
	}
	norm := func(s string) string {
		s = strings.ToLower(s)
		for _, p := range []string{"naa.", "eui.", "0x"} {
			s = strings.TrimPrefix(s, p)
		}
		return s
	}
	return norm(wwid) == norm(target)
}

// describe names a slot and its disk for messages.
func describe(s *Slot) string {
	if s.Disk != nil && s.Disk.Block != "" {
		return fmt.Sprintf("%s (%s)", s.Label, s.Disk.Block)
	}
	return s.Label
}
