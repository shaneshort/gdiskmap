package layout

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// simulatedUser answers calibration prompts from the true slot-to-bay
// mapping, as a person looking at the lit bay would.
type simulatedUser struct {
	// truth maps enclosure index → slot → bay.
	truth []map[int]int
	// hidden holds slots of the first enclosure whose LED the user cannot
	// see, so they skip them.
	hidden map[int]bool
	// quitAfter, if positive, quits on that prompt number.
	quitAfter int
	asks      int
	confirms  int
	notices   []string
}

func (u *simulatedUser) prompts() int { return u.asks + u.confirms }

func (u *simulatedUser) AskBay(s SlotRef) (Response, error) {
	u.asks++
	if u.quitAfter > 0 && u.prompts() >= u.quitAfter {
		return Response{Kind: RespQuit}, nil
	}
	if s.Enclosure == 0 && u.hidden[s.Slot] {
		return Response{Kind: RespSkip}, nil
	}
	return Response{Kind: RespBay, Bay: u.truth[s.Enclosure][s.Slot]}, nil
}

func (u *simulatedUser) ConfirmBay(s SlotRef, predicted int) (Response, error) {
	u.confirms++
	if u.quitAfter > 0 && u.prompts() >= u.quitAfter {
		return Response{Kind: RespQuit}, nil
	}
	b := u.truth[s.Enclosure][s.Slot]
	switch {
	case s.Enclosure == 0 && u.hidden[s.Slot]:
		return Response{Kind: RespSkip}, nil
	case b == predicted:
		return Response{Kind: RespYes}, nil
	default:
		return Response{Kind: RespBay, Bay: b}, nil
	}
}

func (u *simulatedUser) Notify(msg string) { u.notices = append(u.notices, msg) }

func slotRange(start, n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = start + i
	}
	return s
}

// reversedPerConnector is the reference host's numbering: within each
// 4-slot connector the firmware counts backwards relative to the bays.
func reversedPerConnector(firstBay int) map[int]int {
	m := make(map[int]int)
	for s := range 8 {
		m[s] = firstBay + (s/4)*4 + 3 - s%4
	}
	return m
}

func TestCalibrate(t *testing.T) {
	tests := []struct {
		name        string
		truth       []map[int]int
		hidden      map[int]bool
		opts        CalibrateOptions
		wantPrompts int
		wantNotices int
	}{
		{
			name:  "forward numbering",
			truth: []map[int]int{{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7}},
			// Two answers and a confirmation for the first group, one
			// confirmation for the second.
			wantPrompts: 4,
		},
		{
			name:        "reversed per connector",
			truth:       []map[int]int{reversedPerConnector(0)},
			wantPrompts: 4,
		},
		{
			name:  "numbering continues across controllers",
			truth: []map[int]int{reversedPerConnector(0), reversedPerConnector(8), reversedPerConnector(16)},
			// The second and third controllers need one confirmation per
			// group.
			wantPrompts: 8,
		},
		{
			name: "second connector numbered independently",
			truth: []map[int]int{{
				0: 0, 1: 1, 2: 2, 3: 3,
				4: 20, 5: 21, 6: 22, 7: 23,
			}},
			// The continuation prediction for slot 7 is corrected to 23,
			// slot 4 is asked, and slot 6 is confirmed.
			wantPrompts: 6,
		},
		{
			name:  "irregular numbering",
			truth: []map[int]int{{0: 5, 1: 2, 2: 7, 3: 0}},
			// Two answers do not form a line, so every slot is asked.
			wantPrompts: 4,
		},
		{
			name:  "confirm all",
			truth: []map[int]int{{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7}},
			opts:  CalibrateOptions{ConfirmAll: true},
			// Two answers, then every other slot is confirmed.
			wantPrompts: 8,
		},
		{
			name:   "invisible LED is inferred",
			truth:  []map[int]int{{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7}},
			hidden: map[int]bool{1: true},
			// Slot 1 is skipped when asked, then slot 2 is asked and slot 3
			// confirmed, which fills in slot 1 from the pattern. The second
			// group needs one confirmation.
			wantPrompts: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := &simulatedUser{truth: tc.truth, hidden: tc.hidden}
			var encls []CalibrationEnclosure
			for i, m := range tc.truth {
				encls = append(encls, CalibrationEnclosure{ID: uint64(i), Slots: slotRange(0, len(m))})
			}
			opts := tc.opts
			opts.GroupSize = 4
			got, quit, err := Calibrate(encls, u, opts)
			if err != nil {
				t.Fatalf("Calibrate() error: %v", err)
			}
			if quit {
				t.Error("Calibrate() quit = true, want false")
			}
			if diff := cmp.Diff(tc.truth, got); diff != "" {
				t.Errorf("Calibrate() mapping mismatch (-want +got):\n%s", diff)
			}
			if u.prompts() != tc.wantPrompts {
				t.Errorf("Calibrate() used %d prompts (%d asks, %d confirms), want %d", u.prompts(), u.asks, u.confirms, tc.wantPrompts)
			}
			if len(u.notices) != tc.wantNotices {
				t.Errorf("Calibrate() notices = %q, want %d", u.notices, tc.wantNotices)
			}
		})
	}
}

func TestCalibrateRejectsTakenBay(t *testing.T) {
	u := &simulatedUser{truth: []map[int]int{{0: 9, 1: 10, 2: 11, 3: 12}}, quitAfter: 6}
	encls := []CalibrationEnclosure{{Slots: slotRange(0, 4)}}
	got, quit, err := Calibrate(encls, u, CalibrateOptions{GroupSize: 4, Taken: map[int]bool{12: true}})
	if err != nil {
		t.Fatalf("Calibrate() error: %v", err)
	}
	if !quit {
		t.Error("Calibrate() quit = false, want true")
	}
	want := []map[int]int{{0: 9, 1: 10, 2: 11}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Calibrate() mapping mismatch (-want +got):\n%s", diff)
	}
	if len(u.notices) == 0 {
		t.Error("Calibrate() gave no notice about the taken bay")
	}
}

func TestCalibrateQuit(t *testing.T) {
	u := &simulatedUser{truth: []map[int]int{reversedPerConnector(0)}, quitAfter: 3}
	encls := []CalibrationEnclosure{{Slots: slotRange(0, 8)}}
	got, quit, err := Calibrate(encls, u, CalibrateOptions{GroupSize: 4})
	if err != nil {
		t.Fatalf("Calibrate() error: %v", err)
	}
	if !quit {
		t.Error("Calibrate() quit = false, want true")
	}
	// The two answers before the quit are kept.
	want := []map[int]int{{0: 3, 1: 2}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Calibrate() mapping mismatch (-want +got):\n%s", diff)
	}
}

func TestCalibrateInvalidGroupSize(t *testing.T) {
	if _, _, err := Calibrate(nil, &simulatedUser{}, CalibrateOptions{}); err == nil {
		t.Error("Calibrate(GroupSize 0) error = nil, want error")
	}
}

func TestFitLine(t *testing.T) {
	tests := []struct {
		known  map[int]int
		want   line
		wantOK bool
	}{
		{map[int]int{0: 3, 1: 2}, line{base: 3, step: -1}, true},
		{map[int]int{1: 5, 3: 7}, line{base: 4, step: 1}, true},
		{map[int]int{0: 0, 1: 1, 3: 2}, line{}, false},
		{map[int]int{0: 0, 2: 1}, line{}, false},
		{map[int]int{0: 0}, line{}, false},
	}
	for _, tc := range tests {
		got, ok := fitLine(tc.known)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("fitLine(%v) = %v, %t, want %v, %t", tc.known, got, ok, tc.want, tc.wantOK)
		}
	}
}
