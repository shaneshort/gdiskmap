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
			// Unusable bays are never lit; empty ones only with -light-empty.
			if s == nil || s.Unusable || (s.Disk == nil && !*lightEmpty) {
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
	if slices.ContainsFunc(sys.inv.Slots, func(s inventory.Slot) bool { return s.Unusable }) {
		fmt.Fprintln(e.stdout, "Slots marked unusable are not lit; their bays are inferred from the pattern.")
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
	ok, err := confirm(e, fmt.Sprintf("Write these labels to %s? Existing labels for these enclosures are replaced. [y/N] ", e.configPath))
	if err != nil || !ok {
		fmt.Fprintln(e.stdout, "Not saved.")
		return err
	}
	for i, t := range targets {
		sys.cfg.ClearLabels(t.enc.ID)
		for slot, bay := range result[i] {
			if err := sys.cfg.SetLabel(t.enc.ID, slot, strconv.Itoa(bay)); err != nil {
				return err
			}
		}
	}
	backup, err := sys.cfg.Save(e.configPath)
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Fprintf(e.stdout, "Saved %s (previous version kept as %s).\n", e.configPath, backup)
	} else {
		fmt.Fprintf(e.stdout, "Saved %s.\n", e.configPath)
	}
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
		p.unlight()
		fmt.Fprintln(p.e.stderr, "\ngdiskmap: interrupted; nothing was saved")
		os.Exit(130)
	}()
}

// stop removes the signal handler and turns off any lit LED.
func (p *terminalPrompter) stop() {
	signal.Stop(p.sigs)
	close(p.sigs)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unlight()
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

// light turns on the identify LED of the slot. A failure is reported but
// does not stop calibration; the user can skip the slot.
func (p *terminalPrompter) light(s *inventory.Slot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unlight()
	if err := setIndicator(p.sys, s, mpt.StatusIdentify, true); err != nil && !errors.Is(err, mpt.ErrStatusNotPreserved) {
		fmt.Fprintf(p.e.stderr, "gdiskmap: %s: turning the LED on failed: %v\n", s.DefaultLabel, err)
		return
	}
	p.lit = s
}

// unlight turns off the lit LED. The caller must hold p.mu.
func (p *terminalPrompter) unlight() {
	if p.lit == nil {
		return
	}
	if err := setIndicator(p.sys, p.lit, mpt.StatusIdentify, false); err != nil && !errors.Is(err, mpt.ErrStatusNotPreserved) {
		fmt.Fprintf(p.e.stderr, "gdiskmap: %s: turning the LED off failed: %v\n", p.lit.DefaultLabel, err)
	}
	p.lit = nil
}

func (p *terminalPrompter) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unlight()
}

// AskBay implements layout.Prompter.
func (p *terminalPrompter) AskBay(ref layout.SlotRef) (layout.Response, error) {
	s := p.slot(ref)
	if s == nil {
		return layout.Response{Kind: layout.RespSkip}, nil
	}
	p.light(s)
	defer p.done()
	for {
		line, err := p.prompt(fmt.Sprintf("%s is lit. Bay number? [number/s/q] ", slotSummary(s)))
		if err != nil {
			return layout.Response{}, err
		}
		if r, ok := parseResponse(line, false); ok {
			return r, nil
		}
		fmt.Fprintln(p.e.stdout, "Enter a bay number, 's' to skip or 'q' to stop.")
	}
}

// ConfirmBay implements layout.Prompter.
func (p *terminalPrompter) ConfirmBay(ref layout.SlotRef, predicted int) (layout.Response, error) {
	s := p.slot(ref)
	if s == nil {
		return layout.Response{Kind: layout.RespSkip}, nil
	}
	p.light(s)
	defer p.done()
	for {
		line, err := p.prompt(fmt.Sprintf("%s is lit. Is it bay %d? [Y/n/number/s/q] ", slotSummary(s), predicted))
		if err != nil {
			return layout.Response{}, err
		}
		if r, ok := parseResponse(line, true); ok {
			return r, nil
		}
		fmt.Fprintln(p.e.stdout, "Enter 'y', 'n', the correct bay number, 's' to skip or 'q' to stop.")
	}
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
