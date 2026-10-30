//go:build !(linux && (amd64 || arm64))

package mpt

import (
	"errors"
	"fmt"
)

// openTransport fails on platforms other than 64-bit Linux: the ioctl
// structure layout depends on the kernel's pointer size, and the ctl device
// exists only on Linux.
func openTransport(dev string, _ int) (transport, error) {
	return nil, fmt.Errorf("opening %s: MPI access requires 64-bit Linux: %w", dev, errors.ErrUnsupported)
}
