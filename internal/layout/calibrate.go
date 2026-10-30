package layout

import (
	"errors"
	"fmt"
)

// ResponseKind identifies the type of answer to a calibration prompt.
type ResponseKind int

// Calibration responses.
const (
	// RespBay gives the bay number of the lit slot (Response.Bay).
	RespBay ResponseKind = iota
	// RespYes confirms a predicted bay number.
	RespYes
	// RespNo rejects a predicted bay number without giving the correct one.
	RespNo
	// RespSkip leaves the slot unmapped, e.g. because no lit bay is visible.
	RespSkip
	// RespQuit stops calibration, keeping the mappings made so far.
	RespQuit
)

// Response is the user's answer to a calibration prompt.
type Response struct {
	Kind ResponseKind
	Bay  int
}

// SlotRef identifies a slot during calibration.
type SlotRef struct {
	// Enclosure indexes the enclosures passed to Calibrate.
	Enclosure int
	Slot      int
}

// Prompter shows slots to the user and collects answers. Implementations
// light the slot's identify LED for the duration of each prompt.
type Prompter interface {
	// AskBay asks which bay is lit. It returns RespBay, RespSkip or
	// RespQuit.
	AskBay(s SlotRef) (Response, error)
	// ConfirmBay asks whether the lit bay is the predicted one. It returns
	// RespYes, RespNo, RespBay (a correction), RespSkip or RespQuit.
	ConfirmBay(s SlotRef, predicted int) (Response, error)
	// Notify shows a message, such as a rejected answer, to the user.
	Notify(msg string)
}

// CalibrationEnclosure is an enclosure to calibrate.
type CalibrationEnclosure struct {
	ID    uint64
	Slots []int // In ascending order.
}

// CalibrateOptions controls calibration.
type CalibrateOptions struct {
	// GroupSize is the number of consecutive slots sharing a numbering
	// pattern, normally the lanes of one connector (4 for SFF-8087/8643).
	GroupSize int
	// ConfirmAll verifies every predicted bay instead of one per group.
	ConfirmAll bool
	// Taken holds bay numbers already in use outside the calibrated
	// enclosures, which answers must not reuse.
	Taken map[int]bool
}

// errQuit stops calibration when the user asks to.
var errQuit = errors.New("calibration stopped")

// Calibrate maps each slot of encls to a bay number by lighting slots and
// asking the user, inferring the numbering pattern of each slot group to
// avoid asking about every slot.
//
// Within a group of opts.GroupSize consecutive slots, bay numbers are
// assumed to run in a straight line: bay = base + d*i for the i-th slot,
// with d = +1 or -1. Two answers determine base and d; one prediction is
// then shown and must be confirmed before the rest of the group is accepted.
// Each following group is predicted to continue the previous group's bay
// numbers in the same direction, again confirmed with one prompt. A rejected
// prediction falls back to asking about each remaining slot of the group.
//
// The result holds one slot-to-bay map per enclosure. quit reports that the
// user stopped early; the result then holds the mappings made so far.
func Calibrate(encls []CalibrationEnclosure, p Prompter, opts CalibrateOptions) (result []map[int]int, quit bool, err error) {
	if opts.GroupSize < 1 {
		return nil, false, fmt.Errorf("invalid group size %d", opts.GroupSize)
	}
	c := &calibrator{p: p, opts: opts, used: make(map[int]bool)}
	for b := range opts.Taken {
		c.used[b] = true
	}
	result = make([]map[int]int, len(encls))
	for ei, e := range encls {
		result[ei] = make(map[int]int)
		for start := 0; start < len(e.Slots); start += opts.GroupSize {
			g := group{enclosure: ei, slots: e.Slots[start:min(start+opts.GroupSize, len(e.Slots))]}
			err := c.calibrateGroup(g, result[ei])
			if errors.Is(err, errQuit) {
				return result, true, nil
			}
			if err != nil {
				return nil, false, err
			}
		}
	}
	return result, false, nil
}

// group is a run of consecutive slots expected to share a numbering pattern.
type group struct {
	enclosure int
	slots     []int
}

func (g group) ref(i int) SlotRef {
	return SlotRef{Enclosure: g.enclosure, Slot: g.slots[i]}
}

// line is a straight-line numbering pattern: the i-th slot of a group is in
// bay base + step*i.
type line struct {
	base, step int
}

func (l line) bay(i int) int { return l.base + l.step*i }

type calibrator struct {
	p    Prompter
	opts CalibrateOptions
	used map[int]bool
	// prev is the pattern of the most recently completed group, with the
	// group's highest bay number, if the group followed a straight line.
	prev    *line
	prevMax int
}

// calibrateGroup maps the slots of g, recording answers in out.
func (c *calibrator) calibrateGroup(g group, out map[int]int) error {
	known := make(map[int]int) // Slot index within g → bay.
	defer func() {
		for i, b := range known {
			out[g.slots[i]] = b
		}
		c.learn(g, known)
	}()

	// Predict this group as a continuation of the previous one.
	if c.prev != nil {
		next := line{base: c.prevMax + 1, step: c.prev.step}
		if next.step < 0 {
			next.base = c.prevMax + len(g.slots)
		}
		if ok, err := c.tryLine(g, next, known); err != nil || ok {
			return err
		}
	}

	// Learn the pattern from two answers.
	for i := range g.slots {
		if len(known) == 2 {
			break
		}
		if _, ok := known[i]; ok {
			continue
		}
		if err := c.ask(g, i, known); err != nil {
			return err
		}
	}
	if l, ok := fitLine(known); ok {
		if ok, err := c.tryLine(g, l, known); err != nil || ok {
			return err
		}
	}

	// No usable pattern: ask about every remaining slot.
	for i := range g.slots {
		if _, ok := known[i]; ok {
			continue
		}
		if err := c.ask(g, i, known); err != nil {
			return err
		}
	}
	return nil
}

// tryLine predicts the unknown slots of g from l and verifies the
// prediction: the last unknown slot (or, with ConfirmAll, every unknown
// slot) is shown for confirmation. It reports whether the prediction was
// accepted and every slot is now known. A rejection records any correction
// in known and returns false.
func (c *calibrator) tryLine(g group, l line, known map[int]int) (bool, error) {
	var unknown []int
	for i := range g.slots {
		if _, ok := known[i]; ok {
			continue
		}
		if b := l.bay(i); b < 0 || c.used[b] {
			// The prediction collides with an existing bay, so it is wrong.
			return false, nil
		}
		unknown = append(unknown, i)
	}
	if len(unknown) == 0 {
		return true, nil
	}
	verify := unknown[len(unknown)-1:]
	if c.opts.ConfirmAll {
		verify = unknown
	}
	for _, i := range verify {
		r, err := c.p.ConfirmBay(g.ref(i), l.bay(i))
		if err != nil {
			return false, err
		}
		switch r.Kind {
		case RespYes:
			c.record(i, l.bay(i), known)
		case RespBay:
			if err := c.recordAnswer(g, i, r.Bay, known); err != nil {
				return false, err
			}
			return false, nil
		case RespNo:
			if err := c.ask(g, i, known); err != nil {
				return false, err
			}
			return false, nil
		case RespSkip:
			return false, nil
		case RespQuit:
			return false, errQuit
		default:
			return false, fmt.Errorf("unexpected response %d to a confirmation", r.Kind)
		}
	}
	for _, i := range unknown {
		c.record(i, l.bay(i), known)
	}
	return true, nil
}

// ask asks for the bay of the i-th slot of g until it gets a usable answer,
// a skip or a quit.
func (c *calibrator) ask(g group, i int, known map[int]int) error {
	r, err := c.p.AskBay(g.ref(i))
	if err != nil {
		return err
	}
	switch r.Kind {
	case RespBay:
		return c.recordAnswer(g, i, r.Bay, known)
	case RespSkip:
		return nil
	case RespQuit:
		return errQuit
	}
	return fmt.Errorf("unexpected response %d to a question", r.Kind)
}

// recordAnswer records a bay number given by the user, asking again if it is
// invalid or already used.
func (c *calibrator) recordAnswer(g group, i, bay int, known map[int]int) error {
	switch {
	case bay < 0:
		c.p.Notify(fmt.Sprintf("Bay numbers cannot be negative (got %d).", bay))
	case c.used[bay]:
		c.p.Notify(fmt.Sprintf("Bay %d is already assigned to another slot.", bay))
	default:
		c.record(i, bay, known)
		return nil
	}
	return c.ask(g, i, known)
}

func (c *calibrator) record(i, bay int, known map[int]int) {
	known[i] = bay
	c.used[bay] = true
}

// learn remembers g's pattern for predicting the next group. Skipped slots
// do not prevent this, but a group whose known bays do not follow a straight
// line clears the prediction.
func (c *calibrator) learn(g group, known map[int]int) {
	l, ok := fitLine(known)
	if !ok {
		c.prev = nil
		return
	}
	c.prev = &l
	c.prevMax = max(l.bay(0), l.bay(len(g.slots)-1))
}

// fitLine returns the straight line with step +1 or -1 through every known
// bay, if there is one and at least two bays are known.
func fitLine(known map[int]int) (line, bool) {
	if len(known) < 2 {
		return line{}, false
	}
	first := -1
	for i := range known {
		if first < 0 || i < first {
			first = i
		}
	}
	second := -1
	for i := range known {
		if i != first && (second < 0 || i < second) {
			second = i
		}
	}
	diff := known[second] - known[first]
	span := second - first
	if diff != span && diff != -span {
		return line{}, false
	}
	step := diff / span
	l := line{base: known[first] - step*first, step: step}
	for i, b := range known {
		if l.bay(i) != b {
			return line{}, false
		}
	}
	return l, true
}
