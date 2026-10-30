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

	"gdiskmap/internal/capture"
	"gdiskmap/internal/inventory"
	"gdiskmap/internal/mpt"
	"gdiskmap/internal/output"
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
	slots, err := sys.inv.Select(args[1:], sys.wholeDisk)
	if err != nil {
		return err
	}
	failed, notPreserved := 0, 0
	for _, s := range slots {
		if err := setIndicator(sys, s, indicator, on); err != nil {
			if !errors.Is(err, mpt.ErrStatusNotPreserved) {
				fmt.Fprintf(e.stderr, "gdiskmap: %s: %v\n", describeSlot(s), err)
				failed++
				continue
			}
			notPreserved++
		}
		fmt.Fprintf(e.stdout, "%s: %s %s\n", describeSlot(s), name, args[0])
	}
	if notPreserved > 0 {
		// Firmware that rejects SEP read-status requests leaves no way to
		// preserve the slot's other bits; sas2ircu behaves the same way.
		fmt.Fprintf(e.stderr, "gdiskmap: warning: the controller cannot report LED state, so setting the %s LED cleared any other indicator on %d slot(s)\n", name, notPreserved)
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
