# MPI access

gdiskmap reads most of its data from sysfs. Two things are only available
from the HBA firmware:

- the slot range of each enclosure, which is needed to list empty slots;
- the slot indicator (LED) state, and setting it.

On a direct-attach backplane there is no SES device; the HBA drives the bay
LEDs itself over SGPIO. The only way to reach them is an MPI (Message Passing
Interface) request to the firmware, which is also how `sas2ircu LOCATE`
works.

## Transport

`mpt3sas` exposes a passthrough ioctl (`MPT3COMMAND`, `_IOWR('L', 20,
struct mpt3_ioctl_command)`) on two character devices:

| Device | Controllers |
|---|---|
| `/dev/mpt2ctl` | SAS2 (MPI 2.0): SAS2008, SAS2308, ... |
| `/dev/mpt3ctl` | SAS3 (MPI 2.5/2.6): SAS3008, SAS3108, SAS3216, ... |

Controllers are addressed by the driver's adapter number, which sysfs
publishes as `/sys/class/scsi_host/hostN/unique_id`. SAS2 and SAS3 adapters
are numbered independently.

The request frame is passed inline after the ioctl structure. The structure
holds user-space pointers to the reply and data buffers, so gdiskmap
allocates them in a single `mmap`'d page outside the Go heap. The structure
layout depends on the pointer size, so MPI access is built only for 64-bit
Linux (amd64 and arm64).

## Requests sent

The `internal/mpt` package can build only these requests. Field layouts and
constants follow the kernel's `drivers/scsi/mpt3sas/mpi/` headers.

| Request | MPI function | Purpose |
|---|---|---|
| CONFIG, action PAGE_HEADER, SAS Enclosure Page 0 | `0x04` | Learn the page length. |
| CONFIG, action READ_CURRENT, SAS Enclosure Page 0, form GET_NEXT_HANDLE | `0x04` | Iterate the enclosures: handle, logical ID, start slot, slot count. |
| SCSI Enclosure Processor, action READ_STATUS | `0x18` | Read a slot's status word (`disks -leds`, and before each write). |
| SCSI Enclosure Processor, action WRITE_STATUS | `0x18` | Set a slot's status word. |

Slots are addressed by enclosure handle and slot number, as `sas2ircu`
does. The firmware device handle (from
`/sys/block/sdX/device/sas_device_handle`) is used only for a disk whose
enclosure handle is unknown, because SAS2 IT firmware (P20) driving an SGPIO
backplane rejects READ_STATUS requests addressed by device handle (IOCStatus
`0x0007`, IOCLogInfo `0x30060001`) and accepts WRITE_STATUS requests
addressed that way without changing the LED.

This was established by tracing `sas2ircu <n> LOCATE <encl>:<slot> ON`: it
sends READ_STATUS then WRITE_STATUS, both with Flags `0x01`
(ENCLOSURE_SLOT_ADDRESS), writing the slot's current status with the
identify bit added (e.g. `0x00000080` becomes `0x00020080`).

A write replaces the whole status word, so gdiskmap reads the status first
and changes only the identify (`0x00020000`) or predicted-fault (`0x40`)
bit. If the read fails, the write still happens and a warning says other
indicators on the slot may have been cleared.

Nothing that writes firmware, NVDATA or persistent configuration can be
built; there is no general passthrough.

## Safety considerations

- Each request has a 10-second firmware timeout. If a passthrough request
  times out, `mpt3sas` resets the controller, which interrupts I/O to every
  disk on it. These requests normally complete in milliseconds, and
  `sas2ircu` is exposed to the same behaviour, but it is the reason
  gdiskmap sends no request it does not need.
- The ctl devices are root-only (mode 0600).
