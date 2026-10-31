package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaneshort/gdiskmap/internal/capture"
	"github.com/shaneshort/gdiskmap/internal/inventory"
	"github.com/shaneshort/gdiskmap/internal/layout"
	"github.com/shaneshort/gdiskmap/internal/mpt"
	"github.com/shaneshort/gdiskmap/internal/output"
)

// newFlagSet returns a flag set for a subcommand that reports errors as
// usage errors instead of exiting.
func newFlagSet(name string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	fl.Usage = func() {}
	return fl
}

func parseFlags(fl *flag.FlagSet, args []string) error {
	if err := fl.Parse(args); err != nil {
		return usageError{fmt.Sprintf("%s: %v", fl.Name(), err)}
	}
	return nil
}

// cmdDisks implements `gdiskmap disks`.
func cmdDisks(e *env, args []string) error {
	fl := newFlagSet("disks")
	asJSON := fl.Bool("json", false, "")
	// Reading LED state sends one MPI request per slot, so it is opt-in: a
	// plain listing sends only the enclosure page reads it needs.
	leds := fl.Bool("leds", false, "")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return usageError{"disks: unexpected arguments"}
	}
	sys, err := gather(e, e.liveFS(), gatherOptions{readLEDs: *leds})
	if err != nil {
		return err
	}
	defer sys.close()
	if *asJSON {
		return output.JSON(e.stdout, sys.inv)
	}
	sys.printWarnings(e)
	return output.Table(e.stdout, sys.inv)
}

// cmdLED implements `gdiskmap locate` and `gdiskmap fault`.
func cmdLED(e *env, args []string, name string, indicator mpt.SlotStatus) error {
	if len(args) < 2 || (args[0] != "on" && args[0] != "off") {
		return usageError{fmt.Sprintf("usage: gdiskmap %s on|off <target>...", name)}
	}
	on := args[0] == "on"
	sys, err := gather(e, e.liveFS(), gatherOptions{needControllers: true})
	if err != nil {
		return err
	}
	defer sys.close()
	sys.printWarnings(e)
	slots, err := sys.inv.Select(args[1:], sys.wholeDisk)
	if err != nil {
		return err
	}
	failed := 0
	for _, s := range slots {
		if err := setIndicator(sys, s, indicator, on); err != nil {
			fmt.Fprintf(e.stderr, "gdiskmap: %s: %v\n", describeSlot(s), err)
			failed++
			continue
		}
		fmt.Fprintf(e.stdout, "%s: %s %s\n", describeSlot(s), name, args[0])
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d slots failed", failed, len(slots))
	}
	return nil
}

// setIndicator turns an indicator of a slot on or off.
func setIndicator(sys *system, s *inventory.Slot, indicator mpt.SlotStatus, on bool) error {
	addr, err := sys.address(s)
	if err != nil {
		return err
	}
	c, err := sys.controller(s)
	if err != nil {
		return err
	}
	return c.SetIndicator(addr, indicator, on)
}

// describeSlot names a slot and its disk for messages, e.g.
// "11 (c0:s3, sdb, S460S1L7)".
func describeSlot(s *inventory.Slot) string {
	var details []string
	if s.Label != s.DefaultLabel {
		details = append(details, s.DefaultLabel)
	}
	if d := s.Disk; d != nil {
		if d.Block != "" {
			details = append(details, d.Block)
		}
		if d.Serial != "" {
			details = append(details, d.Serial)
		}
	} else {
		details = append(details, "empty")
	}
	if len(details) == 0 {
		return s.Label
	}
	return fmt.Sprintf("%s (%s)", s.Label, strings.Join(details, ", "))
}

// cmdUnusable implements `gdiskmap unusable`, which marks slots as unusable
// (or clears the mark) in the layout config.
func cmdUnusable(e *env, args []string) error {
	const usage = "usage: gdiskmap unusable on|off [-reason text] <target>..."
	if len(args) < 1 || (args[0] != "on" && args[0] != "off") {
		return usageError{usage}
	}
	on := args[0] == "on"
	fl := newFlagSet("unusable")
	reason := fl.String("reason", "", "")
	if err := parseFlags(fl, args[1:]); err != nil {
		return err
	}
	if fl.NArg() == 0 {
		return usageError{usage}
	}
	if !on && *reason != "" {
		return usageError{"unusable off does not take -reason"}
	}
	sys, err := gather(e, e.liveFS(), gatherOptions{})
	if err != nil {
		return err
	}
	defer sys.close()
	sys.printWarnings(e)
	slots, err := sys.inv.Select(fl.Args(), sys.wholeDisk)
	if err != nil {
		return err
	}
	type mark struct {
		id   uint64
		slot int
	}
	var marks []mark
	for _, s := range slots {
		if s.Enclosure < 0 {
			return fmt.Errorf("%s has no known slot to mark", describeSlot(s))
		}
		marks = append(marks, mark{sys.inv.Controllers[s.Controller].Enclosures[s.Enclosure].ID, s.Number})
	}
	backup, err := layout.Update(e.configPath, func(c *layout.Config) error {
		for _, m := range marks {
			if !on {
				c.ClearUnusable(m.id, m.slot)
				continue
			}
			if err := c.SetUnusable(m.id, m.slot, *reason); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, s := range slots {
		fmt.Fprintf(e.stdout, "%s: unusable %s\n", describeSlot(s), args[0])
		if on && s.Disk != nil {
			fmt.Fprintf(e.stderr, "gdiskmap: warning: %s holds a disk\n", describeSlot(s))
		}
	}
	reportSaved(e, backup)
	return nil
}

// reportSaved tells the user the config was saved, and where the previous
// version was kept.
func reportSaved(e *env, backup string) {
	if backup != "" {
		fmt.Fprintf(e.stdout, "Saved %s (previous version kept as %s).\n", e.configPath, backup)
	} else {
		fmt.Fprintf(e.stdout, "Saved %s.\n", e.configPath)
	}
}

// cmdDump implements `gdiskmap dump`.
func cmdDump(e *env, args []string) error {
	if len(args) != 1 {
		return usageError{"usage: gdiskmap dump <dir>"}
	}
	if e.root != "" {
		return usageError{"dump reads the live system and cannot be combined with -root"}
	}
	dir := args[0]
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	rec := capture.NewRecorder(e.liveFS())
	sys, err := gather(e, rec, gatherOptions{})
	if err != nil {
		return err
	}
	defer sys.close()
	if err := rec.WriteTo(dir); err != nil {
		return fmt.Errorf("writing capture: %w", err)
	}
	if sys.zpoolJSON != nil {
		if err := os.WriteFile(filepath.Join(dir, dumpZpoolFile), sys.zpoolJSON, 0o644); err != nil {
			return err
		}
	}
	if len(sys.enclosures) > 0 {
		data, err := json.MarshalIndent(sys.enclosures, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, dumpEnclosuresFile), append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.stdout, "Captured %d controllers into %s.\nReplay with: gdiskmap -root %s disks\n", len(sys.inv.Controllers), dir, dir)
	return nil
}
