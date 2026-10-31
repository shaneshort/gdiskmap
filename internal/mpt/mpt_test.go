package mpt

import (
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestIOWR(t *testing.T) {
	// MPT3COMMAND on 64-bit Linux, as computed by the kernel's _IOWR macro.
	const want = 0xc0484c14
	if got := iowr('L', 20, 72); got != want {
		t.Errorf("iowr('L', 20, 72) = %#x, want %#x", got, want)
	}
}

func TestEncodeSEPRequest(t *testing.T) {
	tests := []struct {
		name   string
		action uint8
		addr   SlotAddress
		status SlotStatus
		want   string
	}{
		{
			name:   "write identify by device handle",
			action: sepActionWriteStatus,
			addr:   DeviceAddress(0x000e),
			status: StatusIdentify,
			// DevHandle 0x000e, Function 0x18, Action 0, Flags 0,
			// SlotStatus 0x00020000.
			want: "0e000018000000000000000000000200" + "0000000000000000000000000000" + "0000",
		},
		{
			name:   "read status by enclosure slot",
			action: sepActionReadStatus,
			addr:   EnclosureSlotAddress(0x0001, 7),
			// Function 0x18, Action 1, Flags 1, Slot 7, EnclosureHandle 1.
			want: "000000180101000000000000000000000000000000000000000000000700" + "0100",
		},
	}
	for _, tc := range tests {
		got := hex.EncodeToString(encodeSEPRequest(tc.action, tc.addr, tc.status))
		if got != tc.want {
			t.Errorf("encodeSEPRequest(%s) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestEncodeEnclosureRequest(t *testing.T) {
	hdr := pageHeader{version: 4, length: 0, number: 0, pageType: pageTypeExtended}
	got := hex.EncodeToString(encodeEnclosureRequest(configActionReadCurrent, hdr, 10, 0xffff))
	// Action 1, Function 0x04, ExtPageLength 10, ExtPageType 0x15,
	// Header {4, 0, 0, 0x0f}, PageAddress 0x0000ffff.
	want := "0100000" + "40a0015" + "00" + "000000000000000000000000" + "0400000f" + "ffff0000"
	if got != want {
		t.Errorf("encodeEnclosureRequest() = %s, want %s", got, want)
	}
	if len(got)/2 != configRequestSize {
		t.Errorf("encodeEnclosureRequest() length = %d, want %d", len(got)/2, configRequestSize)
	}
}

func TestCheckReply(t *testing.T) {
	reply := func(function uint8, status uint16) []byte {
		b := make([]byte, 0x20)
		b[0x03] = function
		le.PutUint16(b[0x0e:], status)
		le.PutUint32(b[0x10:], 0x31120000)
		return b
	}
	if err := checkReply(reply(functionSEP, 0x8000), functionSEP); err != nil {
		t.Errorf("checkReply(success with log info flag) = %v, want nil", err)
	}
	var se *IOCStatusError
	if err := checkReply(reply(functionSEP, 0x0007), functionSEP); !errors.As(err, &se) || se.Status != 0x0007 || se.LogInfo != 0x31120000 {
		t.Errorf("checkReply(invalid field) = %v, want IOCStatusError 0x0007", err)
	}
	if err := checkReply(reply(functionConfig, 0), functionSEP); err == nil {
		t.Error("checkReply(wrong function) = nil, want error")
	}
	if err := checkReply(make([]byte, 4), functionSEP); err == nil {
		t.Error("checkReply(short) = nil, want error")
	}
}

// fakeFirmware implements transport by emulating the firmware's handling of
// enclosure page and SEP requests, and records the requests it receives.
type fakeFirmware struct {
	enclosures []Enclosure
	// status holds slot status words keyed by "e<handle>:<slot>"; devices
	// maps a device handle to its key.
	status  map[string]SlotStatus
	devices map[uint16]string
	// failReads makes SEP read-status requests fail.
	failReads bool
	// transportErr, if set, is returned for every request, as an ioctl
	// failure would be.
	transportErr error
	requests     int
	writes       []SlotStatus
}

func (f *fakeFirmware) close() error { return nil }

func (f *fakeFirmware) submit(frame, dataIn []byte) ([]byte, error) {
	f.requests++
	if f.transportErr != nil {
		return nil, f.transportErr
	}
	reply := make([]byte, 0x20)
	reply[0x03] = frame[0x03]
	setStatus := func(s uint16) { le.PutUint16(reply[0x0e:], s) }
	switch frame[0x03] {
	case functionConfig:
		if len(frame) != configRequestSize || frame[0x06] != extPageTypeEnclosure {
			return nil, fmt.Errorf("unexpected config frame %x", frame)
		}
		switch frame[0x00] {
		case configActionPageHeader:
			le.PutUint16(reply[0x04:], enclosurePage0Len/4)
			copy(reply[0x14:], []byte{enclosurePage0Version, 0, 0, pageTypeExtended})
		case configActionReadCurrent:
			if len(dataIn) != enclosurePage0Len {
				return nil, fmt.Errorf("data-in length %d, want %d", len(dataIn), enclosurePage0Len)
			}
			prev := uint16(le.Uint32(frame[0x18:]))
			for _, e := range f.enclosures {
				if prev == enclosureFirstHandle || e.Handle > prev {
					le.PutUint64(dataIn[0x0c:], e.LogicalID)
					le.PutUint16(dataIn[0x16:], e.Handle)
					le.PutUint16(dataIn[0x18:], e.NumSlots)
					le.PutUint16(dataIn[0x1a:], e.StartSlot)
					return reply, nil
				}
			}
			setStatus(iocStatusConfigInvalidPage)
		}
	case functionSEP:
		if len(frame) != sepRequestSize {
			return nil, fmt.Errorf("unexpected SEP frame length %d", len(frame))
		}
		key := fmt.Sprintf("e%d:%d", le.Uint16(frame[0x1e:]), le.Uint16(frame[0x1c:]))
		if frame[0x05] == sepFlagsDevHandle {
			var ok bool
			if key, ok = f.devices[le.Uint16(frame[0x00:])]; !ok {
				setStatus(0x0007) // MPI2_IOCSTATUS_INVALID_FIELD.
				return reply, nil
			}
		}
		switch frame[0x04] {
		case sepActionReadStatus:
			if f.failReads {
				setStatus(0x0007)
				return reply, nil
			}
			le.PutUint32(reply[0x14:], uint32(f.status[key]))
		case sepActionWriteStatus:
			s := SlotStatus(le.Uint32(frame[0x0c:]))
			f.status[key] = s
			f.writes = append(f.writes, s)
		}
	default:
		return nil, fmt.Errorf("unexpected MPI function 0x%02x", frame[0x03])
	}
	return reply, nil
}

func TestEnclosures(t *testing.T) {
	want := []Enclosure{
		{Handle: 1, LogicalID: 0x500605b0071f24b0, StartSlot: 0, NumSlots: 8},
		{Handle: 2, LogicalID: 0x500605b0071f24b1, StartSlot: 28, NumSlots: 8},
	}
	c := &Controller{t: &fakeFirmware{enclosures: want}}
	got, err := c.Enclosures()
	if err != nil {
		t.Fatalf("Enclosures() error: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Enclosures() mismatch (-want +got):\n%s", diff)
	}
}

func TestSetIndicator(t *testing.T) {
	newFirmware := func() *fakeFirmware {
		return &fakeFirmware{
			status:  map[string]SlotStatus{"e1:3": StatusPredictedFault | 0x1},
			devices: map[uint16]string{0x0e: "e1:3"},
		}
	}
	tests := []struct {
		name string
		addr SlotAddress
		flag SlotStatus
		on   bool
		want SlotStatus
	}{
		{"identify on preserves fault", DeviceAddress(0x0e), StatusIdentify, true, StatusIdentify | StatusPredictedFault | 0x1},
		{"fault off preserves other bits", EnclosureSlotAddress(1, 3), StatusPredictedFault, false, 0x1},
		{"identify off when already off", DeviceAddress(0x0e), StatusIdentify, false, StatusPredictedFault | 0x1},
	}
	for _, tc := range tests {
		fw := newFirmware()
		c := &Controller{t: fw}
		if err := c.SetIndicator(tc.addr, tc.flag, tc.on); err != nil {
			t.Errorf("SetIndicator(%s) error: %v", tc.name, err)
			continue
		}
		if got := fw.status["e1:3"]; got != tc.want {
			t.Errorf("SetIndicator(%s) status = %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

func TestSetIndicatorUnreadableStatus(t *testing.T) {
	fw := &fakeFirmware{status: map[string]SlotStatus{"e1:3": StatusPredictedFault}, devices: map[uint16]string{0x0e: "e1:3"}, failReads: true}
	c := &Controller{t: fw}
	var se *IOCStatusError
	if err := c.SetIndicator(DeviceAddress(0x0e), StatusIdentify, true); !errors.As(err, &se) {
		t.Fatalf("SetIndicator() error = %v, want IOCStatusError", err)
	}
	// Writing without the current status would clear the fault indicator.
	if len(fw.writes) != 0 {
		t.Errorf("SetIndicator() wrote %#x after a failed read, want no write", fw.writes)
	}
}

func TestTransportFailureStopsRequests(t *testing.T) {
	fw := &fakeFirmware{status: map[string]SlotStatus{}, transportErr: errors.New("timed out")}
	c := &Controller{t: fw}
	if err := c.SetIndicator(EnclosureSlotAddress(1, 0), StatusIdentify, true); err == nil || errors.Is(err, ErrControllerFailed) {
		t.Fatalf("first SetIndicator() error = %v, want the transport error", err)
	}
	fw.transportErr = nil
	if err := c.SetIndicator(EnclosureSlotAddress(1, 1), StatusIdentify, true); !errors.Is(err, ErrControllerFailed) {
		t.Errorf("SetIndicator() after a transport failure error = %v, want ErrControllerFailed", err)
	}
	if _, err := c.Enclosures(); !errors.Is(err, ErrControllerFailed) {
		t.Errorf("Enclosures() after a transport failure error = %v, want ErrControllerFailed", err)
	}
	if fw.requests != 1 {
		t.Errorf("transport received %d requests, want 1", fw.requests)
	}
}

func TestSetIndicatorUnknownDevice(t *testing.T) {
	c := &Controller{t: &fakeFirmware{status: map[string]SlotStatus{}, devices: map[uint16]string{}}}
	var se *IOCStatusError
	if err := c.SetIndicator(DeviceAddress(0x99), StatusIdentify, true); !errors.As(err, &se) {
		t.Errorf("SetIndicator(unknown handle) error = %v, want IOCStatusError", err)
	}
}
