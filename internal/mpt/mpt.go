// Package mpt sends a restricted set of MPI (Message Passing Interface)
// requests to LSI SAS HBAs through the mpt3sas driver's ctl device
// (/dev/mpt2ctl or /dev/mpt3ctl).
//
// Only two kinds of request can be built:
//
//   - CONFIG page-header and read-current requests for SAS Enclosure Page 0,
//     which describes each enclosure's handle and slot range.
//   - SCSI Enclosure Processor (SEP) read-status and write-status requests,
//     which read and set a slot's indicator LEDs.
//
// There is no general passthrough. Requests that write firmware, NVDATA or
// persistent configuration cannot be constructed with this package.
//
// Message layouts and constants follow the MPI 2.0 headers shipped with the
// Linux kernel in drivers/scsi/mpt3sas/mpi/ (mpi2.h, mpi2_cnfg.h and
// mpi2_init.h). All MPI fields are little-endian.
package mpt

import (
	"encoding/binary"
	"errors"
	"fmt"

	"gdiskmap/internal/sysfs"
)

// SlotStatus is a SEP slot status word (the SlotStatus field of
// MPI2_SEP_REQUEST and MPI2_SEP_REPLY).
type SlotStatus uint32

// Slot status bits used by gdiskmap.
const (
	// StatusIdentify requests the identify (locate) indicator.
	// MPI2_SEP_REQ_SLOTSTATUS_IDENTIFY_REQUEST.
	StatusIdentify SlotStatus = 0x00020000
	// StatusPredictedFault requests the fault indicator.
	// MPI2_SEP_REQ_SLOTSTATUS_PREDICTED_FAULT.
	StatusPredictedFault SlotStatus = 0x00000040
)

// Has reports whether every bit in flag is set in s.
func (s SlotStatus) Has(flag SlotStatus) bool {
	return s&flag == flag
}

// Enclosure is the subset of SAS Enclosure Page 0
// (MPI2_CONFIG_PAGE_SAS_ENCLOSURE_0) that gdiskmap uses.
type Enclosure struct {
	// Handle is the firmware's enclosure handle, used to address empty
	// slots.
	Handle uint16 `json:"handle"`
	// LogicalID is the enclosure logical ID. It matches the
	// enclosure_identifier sysfs attribute of devices in the enclosure.
	LogicalID uint64 `json:"logicalId"`
	// StartSlot is the number of the enclosure's first slot.
	StartSlot uint16 `json:"startSlot"`
	// NumSlots is the number of slots in the enclosure.
	NumSlots uint16 `json:"numSlots"`
}

// SlotAddress identifies the slot a SEP request applies to, either through
// the device in it or through its enclosure and slot number.
type SlotAddress struct {
	// BySlot selects enclosure/slot addressing. Otherwise DevHandle is used.
	BySlot          bool
	DevHandle       uint16
	EnclosureHandle uint16
	Slot            uint16
}

// DeviceAddress addresses the slot holding the device with the given firmware
// device handle. SAS2 IT firmware driving an SGPIO backplane does not act on
// SEP requests addressed this way (it rejects reads and ignores writes), so
// EnclosureSlotAddress is preferred whenever the enclosure handle is known.
func DeviceAddress(devHandle uint16) SlotAddress {
	return SlotAddress{DevHandle: devHandle}
}

// EnclosureSlotAddress addresses a slot by enclosure handle and slot number.
// This is the addressing sas2ircu uses, and the only way to address an empty
// slot.
func EnclosureSlotAddress(enclosureHandle, slot uint16) SlotAddress {
	return SlotAddress{BySlot: true, EnclosureHandle: enclosureHandle, Slot: slot}
}

func (a SlotAddress) String() string {
	if a.BySlot {
		return fmt.Sprintf("enclosure 0x%04x slot %d", a.EnclosureHandle, a.Slot)
	}
	return fmt.Sprintf("device handle 0x%04x", a.DevHandle)
}

// IOCStatusError reports a request the firmware completed with a status
// other than success.
type IOCStatusError struct {
	// Function is the MPI function of the failed request.
	Function uint8
	// Status is the IOCStatus value, without the log-info-available flag.
	Status uint16
	// LogInfo is the IOCLogInfo value, which encodes firmware-specific
	// detail.
	LogInfo uint32
}

func (e *IOCStatusError) Error() string {
	return fmt.Sprintf("MPI function 0x%02x failed: IOCStatus 0x%04x, IOCLogInfo 0x%08x", e.Function, e.Status, e.LogInfo)
}

// MPI constants from mpi2.h, mpi2_cnfg.h and mpi2_init.h.
const (
	functionConfig = 0x04 // MPI2_FUNCTION_CONFIG
	functionSEP    = 0x18 // MPI2_FUNCTION_SCSI_ENCLOSURE_PROCESSOR

	iocStatusMask              = 0x7fff // MPI2_IOCSTATUS_MASK
	iocStatusSuccess           = 0x0000 // MPI2_IOCSTATUS_SUCCESS
	iocStatusConfigInvalidPage = 0x0022 // MPI2_IOCSTATUS_CONFIG_INVALID_PAGE

	configActionPageHeader  = 0x00 // MPI2_CONFIG_ACTION_PAGE_HEADER
	configActionReadCurrent = 0x01 // MPI2_CONFIG_ACTION_PAGE_READ_CURRENT
	pageTypeExtended        = 0x0f // MPI2_CONFIG_PAGETYPE_EXTENDED
	extPageTypeEnclosure    = 0x15 // MPI2_CONFIG_EXTPAGETYPE_ENCLOSURE
	enclosurePage0Version   = 0x04 // MPI2_SASENCLOSURE0_PAGEVERSION
	// enclosureFormGetNextHandle is MPI2_SAS_ENCLOS_PGAD_FORM_GET_NEXT_HANDLE.
	// The page address is this form ORed with the previous handle; 0xffff
	// starts the iteration.
	enclosureFormGetNextHandle = 0x00000000
	enclosureFirstHandle       = 0xffff

	sepActionWriteStatus = 0x00 // MPI2_SEP_REQ_ACTION_WRITE_STATUS
	sepActionReadStatus  = 0x01 // MPI2_SEP_REQ_ACTION_READ_STATUS
	sepFlagsDevHandle    = 0x00 // MPI2_SEP_REQ_FLAGS_DEVHANDLE_ADDRESS
	sepFlagsEnclosure    = 0x01 // MPI2_SEP_REQ_FLAGS_ENCLOSURE_SLOT_ADDRESS
)

// Request frame sizes, measured up to the scatter-gather list. The driver
// copies exactly this many bytes of the frame and appends its own SGL.
const (
	configRequestSize = 0x1c // Offset of PageBufferSGE in MPI2_CONFIG_REQUEST.
	sepRequestSize    = 0x20 // sizeof(MPI2_SEP_REQUEST); it has no SGL.
	enclosurePage0Len = 0x20 // Bytes of MPI2_CONFIG_PAGE_SAS_ENCLOSURE_0 decoded.
	// maxEnclosures bounds the enclosure iteration in case firmware never
	// reports the end of the list.
	maxEnclosures = 256
)

var le = binary.LittleEndian

// transport submits one request frame to a controller and returns the reply
// frame. dataIn, if non-empty, receives the request's data-in transfer.
type transport interface {
	submit(frame, dataIn []byte) (reply []byte, err error)
	close() error
}

// Controller sends requests to one controller.
type Controller struct {
	t transport
}

// DevicePath returns the ctl device for controllers using the given sysfs
// driver name.
func DevicePath(driver string) (string, error) {
	switch driver {
	case sysfs.DriverSAS2:
		return "/dev/mpt2ctl", nil
	case sysfs.DriverSAS3:
		return "/dev/mpt3ctl", nil
	}
	return "", fmt.Errorf("unsupported driver %q", driver)
}

// Open opens the controller with the given driver name and IOC number (see
// sysfs.Host). It requires permission to open the ctl device, which normally
// means root.
func Open(driver string, ioc int) (*Controller, error) {
	dev, err := DevicePath(driver)
	if err != nil {
		return nil, err
	}
	t, err := openTransport(dev, ioc)
	if err != nil {
		return nil, err
	}
	return &Controller{t: t}, nil
}

// Close releases the controller.
func (c *Controller) Close() error {
	return c.t.close()
}

// Enclosures returns every enclosure the controller reports, including its
// own virtual enclosure on direct-attach backplanes.
func (c *Controller) Enclosures() ([]Enclosure, error) {
	hdr := pageHeader{version: enclosurePage0Version, pageType: pageTypeExtended}
	reply, err := c.t.submit(encodeEnclosureRequest(configActionPageHeader, hdr, 0, 0), nil)
	if err != nil {
		return nil, fmt.Errorf("reading enclosure page header: %w", err)
	}
	if err := checkReply(reply, functionConfig); err != nil {
		return nil, fmt.Errorf("reading enclosure page header: %w", err)
	}
	hdr = decodePageHeader(reply[0x14:])
	extLen := le.Uint16(reply[0x04:])
	if extLen == 0 {
		return nil, errors.New("reading enclosure page header: firmware reported a zero page length")
	}

	var encls []Enclosure
	handle := uint16(enclosureFirstHandle)
	for range maxEnclosures {
		data := make([]byte, int(extLen)*4)
		req := encodeEnclosureRequest(configActionReadCurrent, hdr, extLen, enclosureFormGetNextHandle|uint32(handle))
		reply, err := c.t.submit(req, data)
		if err != nil {
			return nil, fmt.Errorf("reading enclosure page 0: %w", err)
		}
		var se *IOCStatusError
		if err := checkReply(reply, functionConfig); errors.As(err, &se) && se.Status == iocStatusConfigInvalidPage {
			// The firmware signals the end of the list this way.
			return encls, nil
		} else if err != nil {
			return nil, fmt.Errorf("reading enclosure page 0: %w", err)
		}
		e, err := decodeEnclosurePage0(data)
		if err != nil {
			return nil, err
		}
		if e.Handle == handle {
			return nil, fmt.Errorf("reading enclosure page 0: firmware returned handle 0x%04x twice", handle)
		}
		encls = append(encls, e)
		handle = e.Handle
	}
	return nil, fmt.Errorf("reading enclosure page 0: more than %d enclosures", maxEnclosures)
}

// SlotStatus reads the current status word of a slot.
func (c *Controller) SlotStatus(a SlotAddress) (SlotStatus, error) {
	reply, err := c.t.submit(encodeSEPRequest(sepActionReadStatus, a, 0), nil)
	if err != nil {
		return 0, fmt.Errorf("reading status of %v: %w", a, err)
	}
	if err := checkReply(reply, functionSEP); err != nil {
		return 0, fmt.Errorf("reading status of %v: %w", a, err)
	}
	return SlotStatus(le.Uint32(reply[0x14:])), nil
}

// SetSlotStatus replaces the status word of a slot.
func (c *Controller) SetSlotStatus(a SlotAddress, s SlotStatus) error {
	reply, err := c.t.submit(encodeSEPRequest(sepActionWriteStatus, a, s), nil)
	if err != nil {
		return fmt.Errorf("writing status of %v: %w", a, err)
	}
	if err := checkReply(reply, functionSEP); err != nil {
		return fmt.Errorf("writing status of %v: %w", a, err)
	}
	return nil
}

// ErrStatusNotPreserved is returned (wrapped) by SetIndicator when the slot's
// current status could not be read, so the write may have cleared other
// indicators on the slot. The requested indicator was still set.
var ErrStatusNotPreserved = errors.New("current slot status unreadable; other indicators on the slot may have been cleared")

// SetIndicator turns the indicator bits in flag on or off while preserving
// the slot's other status bits. A write replaces the whole status word, so
// the current status is read first.
func (c *Controller) SetIndicator(a SlotAddress, flag SlotStatus, on bool) error {
	cur, readErr := c.SlotStatus(a)
	if readErr != nil {
		cur = 0
	}
	next := cur &^ flag
	if on {
		next |= flag
	}
	if err := c.SetSlotStatus(a, next); err != nil {
		return err
	}
	if readErr != nil {
		return fmt.Errorf("%w (%w)", ErrStatusNotPreserved, readErr)
	}
	return nil
}

// pageHeader is MPI2_CONFIG_PAGE_HEADER.
type pageHeader struct {
	version  uint8
	length   uint8
	number   uint8
	pageType uint8
}

func decodePageHeader(b []byte) pageHeader {
	return pageHeader{version: b[0], length: b[1], number: b[2], pageType: b[3]}
}

// encodeEnclosureRequest builds an MPI2_CONFIG_REQUEST for SAS Enclosure
// Page 0, up to (not including) PageBufferSGE.
func encodeEnclosureRequest(action uint8, hdr pageHeader, extLen uint16, pageAddr uint32) []byte {
	b := make([]byte, configRequestSize)
	b[0x00] = action
	b[0x03] = functionConfig
	le.PutUint16(b[0x04:], extLen)
	b[0x06] = extPageTypeEnclosure
	b[0x14] = hdr.version
	b[0x15] = hdr.length
	b[0x16] = hdr.number
	b[0x17] = hdr.pageType
	le.PutUint32(b[0x18:], pageAddr)
	return b
}

// encodeSEPRequest builds an MPI2_SEP_REQUEST.
func encodeSEPRequest(action uint8, a SlotAddress, s SlotStatus) []byte {
	b := make([]byte, sepRequestSize)
	b[0x03] = functionSEP
	b[0x04] = action
	if a.BySlot {
		b[0x05] = sepFlagsEnclosure
		le.PutUint16(b[0x1c:], a.Slot)
		le.PutUint16(b[0x1e:], a.EnclosureHandle)
	} else {
		b[0x05] = sepFlagsDevHandle
		le.PutUint16(b[0x00:], a.DevHandle)
	}
	le.PutUint32(b[0x0c:], uint32(s))
	return b
}

// checkReply validates a reply frame's function and IOCStatus. The CONFIG
// and SEP replies share the default reply layout for these fields.
func checkReply(reply []byte, function uint8) error {
	if len(reply) < 0x18 {
		return fmt.Errorf("reply too short: %d bytes", len(reply))
	}
	if reply[0x03] != function {
		return fmt.Errorf("reply is for MPI function 0x%02x, want 0x%02x", reply[0x03], function)
	}
	status := le.Uint16(reply[0x0e:]) & iocStatusMask
	if status != iocStatusSuccess {
		return &IOCStatusError{Function: function, Status: status, LogInfo: le.Uint32(reply[0x10:])}
	}
	return nil
}

// decodeEnclosurePage0 decodes the fields of SAS Enclosure Page 0 that
// gdiskmap uses.
func decodeEnclosurePage0(b []byte) (Enclosure, error) {
	if len(b) < enclosurePage0Len {
		return Enclosure{}, fmt.Errorf("enclosure page 0 too short: %d bytes", len(b))
	}
	return Enclosure{
		LogicalID: le.Uint64(b[0x0c:]),
		Handle:    le.Uint16(b[0x16:]),
		NumSlots:  le.Uint16(b[0x18:]),
		StartSlot: le.Uint16(b[0x1a:]),
	}, nil
}
