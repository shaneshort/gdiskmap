package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/shaneshort/gdiskmap/internal/inventory"
	"github.com/shaneshort/gdiskmap/internal/layout"
	"github.com/shaneshort/gdiskmap/internal/mpt"
)

// cmdCalibrate implements `gdiskmap calibrate`.
func cmdCalibrate(e *env, args []string) error {
	fl := newFlagSet("calibrate")
	groupSize := fl.Int("group-size", 4, "")
	confirmAll := fl.Bool("confirm-all", false, "")
	// Many SGPIO backplanes light only occupied bays, so empty slots are
	// mapped by inference unless the user says their LEDs work.
	lightEmpty := fl.Bool("light-empty", false, "")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	sys, err := gather(e, e.liveFS(), gatherOptions{needControllers: true})
	if err != nil {
		return err
	}
	defer sys.close()
	sys.printWarnings(e)

	targets, err := calibrationTargets(sys.inv, fl.Args())
	if err != nil {
		return err
	}
	encls := make([]layout.CalibrationEnclosure, len(targets))
	calibrated := make(map[uint64]bool)
	for i, t := range targets {
		encls[i] = layout.CalibrationEnclosure{ID: t.enc.ID, Slots: t.slots, Hidden: make(map[int]bool)}
		for _, n := range t.slots {
			s := findSlot(sys.inv, t.controller, t.enclosure, n)
			// Unusable bays and slots whose controller reports several
			// devices (see inventory.Slot.Conflict) are never lit; empty
			// ones only with -light-empty.
			if s == nil || s.Unusable || s.Conflict || (s.Disk == nil && !*lightEmpty) {
				encls[i].Hidden[n] = true
			}
		}
		calibrated[t.enc.ID] = true
	}
	// Bay numbers labelled in other enclosures must not be reused.
	taken := make(map[int]bool)
	for id, labels := range sys.cfg.Labels() {
		if calibrated[id] {
			continue
		}
		for _, l := range labels {
			if n, err := strconv.Atoi(l); err == nil {
				taken[n] = true
			}
		}
	}

	fmt.Fprintf(e.stdout, "Calibrating %d enclosure(s). Each prompt lights one slot's locate LED.\n", len(targets))
	fmt.Fprintln(e.stdout, "Answer with the bay number you see lit, 's' to skip a slot whose LED you cannot see, or 'q' to stop.")
	if !*lightEmpty {
		fmt.Fprintln(e.stdout, "Empty slots are not lit; their bays are inferred from the pattern (use -light-empty if your backplane lights empty bays).")
	}
	if slices.ContainsFunc(sys.inv.Slots, func(s inventory.Slot) bool { return s.Unusable || s.Conflict }) {
		fmt.Fprintln(e.stdout, "Slots marked unusable, and slots the controller reports more than one device in, are not lit; their bays are inferred from the pattern.")
	}
	p := newTerminalPrompter(e, sys, targets)
	p.start()
	result, quit, err := layout.Calibrate(encls, p, layout.CalibrateOptions{
		GroupSize:  *groupSize,
		ConfirmAll: *confirmAll,
		Taken:      taken,
	})
	p.stop()
	if err != nil {
		return err
	}
	if quit {
		fmt.Fprintln(e.stdout, "Stopped; the bays mapped so far are kept.")
	}

	total := 0
	fmt.Fprintln(e.stdout, "\nResult:")
	for i, t := range targets {
		fmt.Fprintf(e.stdout, "  %s enclosure %s:\n", sys.inv.Controllers[t.controller].Name, layout.FormatID(t.enc.ID))
		for _, slot := range t.slots {
			bay, ok := result[i][slot]
			if !ok {
				fmt.Fprintf(e.stdout, "    slot %d: (not mapped)\n", slot)
				continue
			}
			note := ""
			if encls[i].Hidden[slot] {
				note = " (not lit, inferred)"
			}
			fmt.Fprintf(e.stdout, "    slot %d: bay %d%s\n", slot, bay, note)
			total++
		}
	}
	if total == 0 {
		fmt.Fprintln(e.stdout, "Nothing to save.")
		return nil
	}
	// All the calibrated enclosures' labels are cleared before any is set,
	// so a bay label may move from one calibrated enclosure to another.
	apply := func(c *layout.Config) error {
		for _, t := range targets {
			c.ClearLabels(t.enc.ID)
		}
		for i, t := range targets {
			for slot, bay := range result[i] {
				if err := c.SetLabel(t.enc.ID, slot, strconv.Itoa(bay)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Check the result against the loaded config before asking, so that a
	// conflict is reported before the user agrees to save.
	if err := apply(sys.cfg); err != nil {
		return fmt.Errorf("the result conflicts with %s: %w", e.configPath, err)
	}
	ok, err := confirm(e, fmt.Sprintf("Write these labels to %s? Existing labels for these enclosures are replaced. [y/N] ", e.configPath))
	if err != nil || !ok {
		fmt.Fprintln(e.stdout, "Not saved.")
		return err
	}
	backup, err := layout.Update(e.configPath, apply)
	if err != nil {
		return err
	}
	reportSaved(e, backup)
	return nil
}

// calibrationTarget is an enclosure selected for calibration.
type calibrationTarget struct {
	controller int // Index into Inventory.Controllers.
	enclosure  int // Index into the controller's Enclosures.
	enc        inventory.Enclosure
	slots      []int
}

// calibrationTargets returns the enclosures of the controllers named in
// selectors (controller name, PCI address or enclosure ID), or of every
// controller if there are none. Each enclosure must have been read from the
// firmware, since otherwise its empty slots are unknown.
func calibrationTargets(inv *inventory.Inventory, selectors []string) ([]calibrationTarget, error) {
	used := make([]bool, len(selectors))
	var targets []calibrationTarget
	for ci, c := range inv.Controllers {
		for ei, enc := range c.Enclosures {
			selected := len(selectors) == 0
			for i, sel := range selectors {
				id, idErr := layout.ParseID(sel)
				if sel == c.Name || sel == c.PCIAddress || (idErr == nil && id == enc.ID) {
					selected, used[i] = true, true
				}
			}
			if !selected {
				continue
			}
			if !enc.FromFirmware {
				return nil, fmt.Errorf("%s: the enclosure list could not be read from the controller, so its slots are unknown", c.Name)
			}
			t := calibrationTarget{controller: ci, enclosure: ei, enc: enc}
			for n := enc.StartSlot; n < enc.StartSlot+enc.NumSlots; n++ {
				t.slots = append(t.slots, n)
			}
			targets = append(targets, t)
		}
	}
	if i := slices.Index(used, false); i >= 0 {
		return nil, usageError{fmt.Sprintf("calibrate: no controller or enclosure matches %q", selectors[i])}
	}
	if len(targets) == 0 {
		return nil, errors.New("no enclosures to calibrate")
	}
	return targets, nil
}

// terminalPrompter implements layout.Prompter on the terminal, lighting
// each slot's identify LED while asking about it.
type terminalPrompter struct {
	e       *env
	sys     *system
	targets []calibrationTarget
	sigs    chan os.Signal

	mu  sync.Mutex
	lit *inventory.Slot // The slot whose LED is on, if any.
}

func newTerminalPrompter(e *env, sys *system, targets []calibrationTarget) *terminalPrompter {
	return &terminalPrompter{e: e, sys: sys, targets: targets}
}

// start installs a signal handler that turns the lit LED off before exiting,
// so an interrupted calibration does not leave a slot blinking.
func (p *terminalPrompter) start() {
	p.sigs = make(chan os.Signal, 1)
	signal.Notify(p.sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, ok := <-p.sigs; !ok {
			return
		}
		p.mu.Lock()
		fmt.Fprintln(p.e.stderr, "\ngdiskmap: interrupted; nothing was saved")
		if err := p.unlight(); err != nil {
			fmt.Fprintf(p.e.stderr, "gdiskmap: %v\n", err)
		}
		os.Exit(130)
	}()
}

// stop removes the signal handler and turns off any lit LED.
func (p *terminalPrompter) stop() {
	signal.Stop(p.sigs)
	close(p.sigs)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.unlight(); err != nil {
		fmt.Fprintf(p.e.stderr, "gdiskmap: %v\n", err)
	}
}

// slot returns the inventory slot a calibration reference refers to.
func (p *terminalPrompter) slot(ref layout.SlotRef) *inventory.Slot {
	t := p.targets[ref.Enclosure]
	return findSlot(p.sys.inv, t.controller, t.enclosure, ref.Slot)
}

// findSlot returns the first slot with the given controller, enclosure and
// number, or nil.
func findSlot(inv *inventory.Inventory, controller, enclosure, number int) *inventory.Slot {
	for i := range inv.Slots {
		s := &inv.Slots[i]
		if s.Controller == controller && s.Enclosure == enclosure && s.Number == number {
			return s
		}
	}
	return nil
}

// light turns on the identify LED of the slot, first turning off the one
// that is lit. On error the LED of s is not on; if the previous LED could not
// be turned off it is still lit (and p.lit still records it).
func (p *terminalPrompter) light(s *inventory.Slot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.unlight(); err != nil {
		return err
	}
	if err := setIndicator(p.sys, s, mpt.StatusIdentify, true); err != nil {
		return fmt.Errorf("%s: turning the LED on failed: %w", s.DefaultLabel, err)
	}
	p.lit = s
	return nil
}

// unlight turns off the lit LED. If that fails, p.lit keeps the slot, so
// that the user is told which LED is still on. The caller must hold p.mu.
func (p *terminalPrompter) unlight() error {
	if p.lit == nil {
		return nil
	}
	if err := setIndicator(p.sys, p.lit, mpt.StatusIdentify, false); err != nil {
		return fmt.Errorf("%s: turning the LED off failed (turn it off with `gdiskmap locate off %s`): %w", p.lit.DefaultLabel, p.lit.DefaultLabel, err)
	}
	p.lit = nil
	return nil
}

func (p *terminalPrompter) done() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unlight()
}

// ask lights the slot ref refers to, asks question until parse accepts the
// answer, and turns the LED off again.
//
// Calibration relies on the user seeing exactly the slot being asked about,
// so an LED that cannot be switched as intended stops it, with one
// exception: if the firmware rejects the request to light a slot (an
// IOCStatus error), no LED is on and the slot is skipped.
func (p *terminalPrompter) ask(ref layout.SlotRef, question func(*inventory.Slot) string, parse func(string) (layout.Response, bool), help string) (r layout.Response, err error) {
	s := p.slot(ref)
	if s == nil {
		return layout.Response{Kind: layout.RespSkip}, nil
	}
	if err := p.light(s); err != nil {
		var se *mpt.IOCStatusError
		if p.lit == nil && errors.As(err, &se) {
			p.Notify(fmt.Sprintf("gdiskmap: %v; skipping the slot", err))
			return layout.Response{Kind: layout.RespSkip}, nil
		}
		return layout.Response{}, err
	}
	defer func() {
		if doneErr := p.done(); doneErr != nil {
			r, err = layout.Response{}, errors.Join(err, doneErr)
		}
	}()
	for {
		line, err := p.prompt(question(s))
		if err != nil {
			return layout.Response{}, err
		}
		if r, ok := parse(line); ok {
			return r, nil
		}
		fmt.Fprintln(p.e.stdout, help)
	}
}

// AskBay implements layout.Prompter.
func (p *terminalPrompter) AskBay(ref layout.SlotRef) (layout.Response, error) {
	return p.ask(ref,
		func(s *inventory.Slot) string {
			return fmt.Sprintf("%s is lit. Bay number? [number/s/q] ", slotSummary(s))
		},
		func(line string) (layout.Response, bool) { return parseResponse(line, false) },
		"Enter a bay number, 's' to skip or 'q' to stop.")
}

// ConfirmBay implements layout.Prompter.
func (p *terminalPrompter) ConfirmBay(ref layout.SlotRef, predicted int) (layout.Response, error) {
	return p.ask(ref,
		func(s *inventory.Slot) string {
			return fmt.Sprintf("%s is lit. Is it bay %d? [Y/n/number/s/q] ", slotSummary(s), predicted)
		},
		func(line string) (layout.Response, bool) { return parseResponse(line, true) },
		"Enter 'y', 'n', the correct bay number, 's' to skip or 'q' to stop.")
}

// Notify implements layout.Prompter.
func (p *terminalPrompter) Notify(msg string) {
	fmt.Fprintln(p.e.stdout, msg)
}

// prompt prints a question and reads one line of input. End of input is
// treated as a request to stop.
func (p *terminalPrompter) prompt(question string) (string, error) {
	fmt.Fprint(p.e.stdout, question)
	line, err := p.e.stdin.ReadString('\n')
	if errors.Is(err, io.EOF) && line == "" {
		fmt.Fprintln(p.e.stdout)
		return "q", nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// parseResponse interprets an answer. For a confirmation, an empty answer
// means yes.
func parseResponse(line string, confirmation bool) (layout.Response, bool) {
	switch strings.ToLower(line) {
	case "s", "skip":
		return layout.Response{Kind: layout.RespSkip}, true
	case "q", "quit":
		return layout.Response{Kind: layout.RespQuit}, true
	}
	if confirmation {
		switch strings.ToLower(line) {
		case "", "y", "yes":
			return layout.Response{Kind: layout.RespYes}, true
		case "n", "no":
			return layout.Response{Kind: layout.RespNo}, true
		}
	}
	if n, err := strconv.Atoi(line); err == nil {
		return layout.Response{Kind: layout.RespBay, Bay: n}, true
	}
	return layout.Response{}, false
}

// slotSummary describes a slot for a calibration prompt.
func slotSummary(s *inventory.Slot) string {
	if s.Disk == nil {
		return fmt.Sprintf("Slot %s (empty)", s.DefaultLabel)
	}
	parts := []string{s.Disk.Block, s.Disk.Model, s.Disk.Serial}
	parts = slices.DeleteFunc(parts, func(p string) bool { return p == "" })
	return fmt.Sprintf("Slot %s (%s)", s.DefaultLabel, strings.Join(parts, " "))
}

// confirm asks a yes/no question; anything but yes means no.
func confirm(e *env, question string) (bool, error) {
	fmt.Fprint(e.stdout, question)
	line, err := e.stdin.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
