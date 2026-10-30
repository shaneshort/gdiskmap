package mpt

// Linux generic ioctl number encoding (include/uapi/asm-generic/ioctl.h),
// used by amd64 and arm64.
const (
	iocNRShift   = 0
	iocTypeShift = 8
	iocSizeShift = 16
	iocDirShift  = 30
	iocWrite     = 1
	iocRead      = 2
)

// iowr returns the number of an ioctl that both reads and writes a structure
// of the given size, i.e. the _IOWR macro.
func iowr(typ, nr, size uint32) uint32 {
	return (iocRead|iocWrite)<<iocDirShift | size<<iocSizeShift | typ<<iocTypeShift | nr<<iocNRShift
}
