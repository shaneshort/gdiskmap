//go:build linux && (amd64 || arm64)

package mpt

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Layout of struct mpt3_ioctl_command (drivers/scsi/mpt3sas/mpt3sas_ctl.h)
// on 64-bit Linux. The request frame (the struct's trailing mf member)
// follows the fixed fields inline.
const (
	cmdIOCNumber     = 0  // hdr.ioc_number
	cmdTimeout       = 12 // timeout, in seconds
	cmdReplyPtr      = 16 // reply_frame_buf_ptr
	cmdDataInPtr     = 24 // data_in_buf_ptr
	cmdMaxReplyBytes = 48 // max_reply_bytes
	cmdDataInSize    = 52 // data_in_size
	cmdSGEOffset     = 64 // data_sge_offset, in 32-bit words
	cmdFrame         = 68 // mf
	cmdStructSize    = 72 // sizeof(struct mpt3_ioctl_command)
)

// Layout of the buffer shared with the kernel. It is a single page so that
// one mmap provides memory outside the Go heap, whose address can safely be
// handed to the kernel.
const (
	bufSize      = 4096
	replyOffset  = 1024
	replyMax     = 256 // Larger than any MPI2 reply frame.
	dataInOffset = 2048
	dataInMax    = bufSize - dataInOffset
)

// commandTimeout is the firmware timeout for each request, in seconds.
// mpt3sas resets the controller if a passthrough request times out, so this
// is generous for requests that normally complete in milliseconds.
const commandTimeout = 10

// ioctlMPTCommand is MPT3COMMAND (and MPT2COMMAND, which shares the number):
// _IOWR('L', 20, struct mpt3_ioctl_command).
var ioctlMPTCommand = iowr('L', 20, cmdStructSize)

// ctlTransport submits requests through a ctl device.
type ctlTransport struct {
	f   *os.File
	ioc uint32
	buf []byte
}

func openTransport(dev string, ioc int) (transport, error) {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	buf, err := unix.Mmap(-1, 0, bufSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("allocating ioctl buffer: %w", err)
	}
	return &ctlTransport{f: f, ioc: uint32(ioc), buf: buf}, nil
}

func (t *ctlTransport) submit(frame, dataIn []byte) ([]byte, error) {
	if len(frame)%4 != 0 || cmdFrame+len(frame) > replyOffset {
		return nil, fmt.Errorf("invalid request frame length %d", len(frame))
	}
	if len(dataIn) > dataInMax {
		return nil, fmt.Errorf("data-in length %d exceeds %d", len(dataIn), dataInMax)
	}
	b := t.buf
	clear(b)
	le.PutUint32(b[cmdIOCNumber:], t.ioc)
	le.PutUint32(b[cmdTimeout:], commandTimeout)
	le.PutUint64(b[cmdReplyPtr:], t.addr(replyOffset))
	le.PutUint32(b[cmdMaxReplyBytes:], replyMax)
	if len(dataIn) > 0 {
		le.PutUint64(b[cmdDataInPtr:], t.addr(dataInOffset))
		le.PutUint32(b[cmdDataInSize:], uint32(len(dataIn)))
	}
	le.PutUint32(b[cmdSGEOffset:], uint32(len(frame)/4))
	copy(b[cmdFrame:], frame)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), uintptr(ioctlMPTCommand), uintptr(unsafe.Pointer(&b[0])))
	runtime.KeepAlive(t.f)
	if errno != 0 {
		return nil, fmt.Errorf("MPT command ioctl on %s: %w", t.f.Name(), errno)
	}
	copy(dataIn, b[dataInOffset:])
	return slices.Clone(b[replyOffset : replyOffset+replyMax]), nil
}

// addr returns the user-space address of buf[off]. The buffer is mmap'd, so
// the garbage collector never moves it.
func (t *ctlTransport) addr(off int) uint64 {
	return uint64(uintptr(unsafe.Pointer(&t.buf[off])))
}

func (t *ctlTransport) close() error {
	unmapErr := unix.Munmap(t.buf)
	if err := t.f.Close(); err != nil {
		return err
	}
	return unmapErr
}
