// Package output renders an inventory as a table or as JSON.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/shaneshort/gdiskmap/internal/inventory"
	"github.com/shaneshort/gdiskmap/internal/zfs"
)

// none marks an absent value in table cells.
const none = "-"

// Table writes the inventory's slots, followed by pool members that were not
// found on any device.
func Table(w io.Writer, inv *inventory.Inventory) error {
	// tabwriter pads every cell, so rows ending in an empty cell would carry
	// trailing spaces; render into a buffer and trim them.
	var buf bytes.Buffer
	if err := table(&buf, inv); err != nil {
		return err
	}
	for line := range strings.Lines(buf.String()) {
		if _, err := io.WriteString(w, strings.TrimRight(line, " \n")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// column is one column of the slot table.
type column struct {
	header string
	cell   func(inventory.Slot) string
}

// slotColumns returns the columns of the slot table. The SLOT column is
// included only if some slot has a custom label, and the pool columns only
// if pool membership is known.
func slotColumns(inv *inventory.Inventory) []column {
	disk := func(f func(*inventory.Disk) string) func(inventory.Slot) string {
		return func(s inventory.Slot) string {
			if s.Disk == nil {
				return none
			}
			return f(s.Disk)
		}
	}
	vdevs := func(f func(zfs.Vdev) string) func(inventory.Slot) string {
		return disk(func(d *inventory.Disk) string {
			if len(d.Vdevs) == 0 {
				return none
			}
			parts := make([]string, len(d.Vdevs))
			for i, v := range d.Vdevs {
				parts[i] = orNone(f(v))
			}
			return strings.Join(parts, ",")
		})
	}

	cols := []column{{"BAY", func(s inventory.Slot) string { return s.Label }}}
	if slices.ContainsFunc(inv.Slots, func(s inventory.Slot) bool { return s.Label != s.DefaultLabel }) {
		cols = append(cols, column{"SLOT", func(s inventory.Slot) string { return s.DefaultLabel }})
	}
	cols = append(cols,
		column{"DEVICE", disk(func(d *inventory.Disk) string { return orNone(d.Block) })},
		column{"MODEL", func(s inventory.Slot) string {
			switch {
			case s.Disk == nil && s.Unusable:
				return "(unusable)"
			case s.Disk == nil:
				return "(empty)"
			case s.Unusable:
				return orNone(s.Disk.Model) + " [unusable bay]"
			}
			return orNone(s.Disk.Model)
		}},
		column{"SERIAL", disk(func(d *inventory.Disk) string { return orNone(d.Serial) })},
		column{"SIZE", disk(func(d *inventory.Disk) string {
			if d.Size == 0 {
				return none
			}
			return HumanSize(d.Size)
		})},
	)
	if inv.PoolsKnown {
		cols = append(cols,
			column{"POOL", vdevs(func(v zfs.Vdev) string { return v.Pool })},
			column{"VDEV", vdevs(zfs.Vdev.Location)},
			column{"STATE", vdevs(func(v zfs.Vdev) string { return v.State })},
		)
	}
	if ledKnown(inv) {
		cols = append(cols, column{"LED", func(s inventory.Slot) string {
			if s.LED == nil {
				return none
			}
			return LEDString(s.LED)
		}})
	}
	cols = append(cols,
		column{"LINK ERRS", func(s inventory.Slot) string {
			if s.Disk == nil || s.Disk.LinkErrors == 0 {
				return ""
			}
			return strconv.FormatUint(s.Disk.LinkErrors, 10)
		}},
	)
	return cols
}

func table(w io.Writer, inv *inventory.Inventory) error {
	cols := slotColumns(inv)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	cells := make([]string, len(cols))
	for i, c := range cols {
		cells[i] = c.header
	}
	fmt.Fprintln(tw, strings.Join(cells, "\t"))
	for _, list := range [][]inventory.Slot{inv.Slots, inv.Unplaced} {
		for _, s := range list {
			for i, c := range cols {
				cells[i] = c.cell(s)
			}
			fmt.Fprintln(tw, strings.Join(cells, "\t"))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(inv.Missing) == 0 {
		return nil
	}
	fmt.Fprintln(w, "\nPool members not found on any device:")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tVDEV\tNAME\tSTATE\tLAST PATH")
	for _, v := range inv.Missing {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Pool, orNone(v.Location()), v.Name, v.State, orNone(v.Path))
	}
	return tw.Flush()
}

// ledKnown reports whether any slot's LED state was read. Some firmware
// cannot report it, and the column is then omitted.
func ledKnown(inv *inventory.Inventory) bool {
	has := func(s inventory.Slot) bool { return s.LED != nil }
	return slices.ContainsFunc(inv.Slots, has) || slices.ContainsFunc(inv.Unplaced, has)
}

// LEDString describes an indicator state, e.g. "off" or "locate,fault".
func LEDString(l *inventory.LEDState) string {
	var on []string
	if l.Identify {
		on = append(on, "locate")
	}
	if l.Fault {
		on = append(on, "fault")
	}
	if len(on) == 0 {
		return "off"
	}
	return strings.Join(on, ",")
}

// JSON writes the inventory as indented JSON.
func JSON(w io.Writer, inv *inventory.Inventory) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(inv)
}

// HumanSize formats a byte count with decimal (SI) units, as drive vendors
// label capacity, e.g. 2000398934016 as "2.0T".
func HumanSize(n uint64) string {
	const units = "KMGTPE"
	if n < 1000 {
		return fmt.Sprintf("%dB", n)
	}
	v := float64(n)
	i := -1
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	return fmt.Sprintf("%.1f%c", v, units[i])
}

func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}
