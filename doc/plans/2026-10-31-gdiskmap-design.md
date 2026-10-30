# gdiskmap v1 design

Date: 2026-10-31

## Purpose

gdiskmap answers two questions on a ZFS host with LSI SAS HBAs:

1. Which physical bay holds a given disk (faulted, unused, or otherwise)?
2. Which bays have no working disk in them?

It also controls the bay LEDs (identify and fault). It replaces the
`sas2ircu`-based [DiskMap](https://github.com/enix/DiskMap), which is
Solaris/illumos-only and depends on a closed, SAS2-only vendor tool.

## Scope

**Target:** Linux, LSI SAS2/SAS3 HBAs driven by `mpt3sas` (which also drives
SAS2 chips via `/dev/mpt2ctl`). A single static Go binary (`CGO_ENABLED=0`).

**In v1:**

- `disks`: inventory of every slot, including empty ones.
- `locate on|off` and `fault on|off`: bay LED control.
- `calibrate`: an interactive wizard that builds the layout config.
- `dump`: capture of the raw inputs for test fixtures and bug reports.
- An optional layout config mapping slots to front-panel bay labels.

**Phase 2:** a ZED (ZFS Event Daemon) hook that sets the fault LED when ZFS
faults a disk and clears it on replacement. A TUI showing the physical
enclosure, driven by the same layout config.

**Not carried over from upstream:** the interactive shell, the discovery cache
(sysfs reads are fast enough that a cache adds staleness without benefit),
`mangle`, `enumerate`, and `sd_timeout` (which writes kernel memory via
`mdb -kw`).

**Out of scope:** SES enclosures (expander backplanes) and other operating
systems. The internal interfaces are kept small so that an SES backend can be
added later without restructuring.

## Background: findings that drive the design

These were observed on the reference host (`pve`: 1x SAS2008, 2x SAS2308,
direct-attach SGPIO backplanes, no expanders, no SES devices).

- **No SES device exists.** LEDs are driven by the HBA over SGPIO, so the only
  way to set them is an MPI SCSI Enclosure Processor (SEP) request through the
  driver's ioctl interface. This is the same mechanism `sas2ircu LOCATE` uses.
  The message formats are public: `drivers/scsi/mpt3sas/mpi/` in the Linux
  kernel.
- **sysfs already exposes most of the mapping.** `mpt3sas` publishes the HBA's
  slot number as `/sys/class/sas_device/end_device-*/bay_identifier`, the
  enclosure logical ID as `enclosure_identifier`, and the device handle as
  `/sys/block/sdX/device/sas_device_handle`. These matched `sas2ircu DISPLAY`
  exactly.
- **sysfs cannot show empty slots.** It only describes attached devices. The
  enclosure's slot range (`StartSlot`, `NumSlots`) must be read from MPI
  Enclosure Page 0.
- **SAS addresses are not unique.** SATA disks receive a synthetic address
  `0x44332211<phy>000000`, which repeats on every controller. Disks are keyed
  by (enclosure logical ID, slot) and matched to ZFS by resolving
  `/dev/disk/by-id` links, never by SAS address.
- **Slot numbers do not follow front-panel order.** Within each 4-lane
  connector the firmware numbers slots in reverse (phy 0 → slot 3 … phy 3 →
  slot 0, phy 4 → slot 7 … phy 7 → slot 4). A label mapping is required.
- **Firmware slot tables can be wrong.** One card carried a generic NVDATA
  image that assigned duplicate slots (28–31) to both connectors. Addressing
  LEDs by device handle rather than by slot keeps occupied bays reachable even
  then; the duplicate is reported as a warning.
- **Controller numbering differs by context.** Linux SCSI host numbers, PCI
  order, and the MPI IOC number are independent. The IOC number (which
  matches `sas2ircu`'s index) is read from
  `/sys/class/scsi_host/hostN/unique_id` and cross-checked against the PCI
  address.
- **Per-phy link error counters are useful.** `/sys/class/sas_phy/phy-*/`
  `invalid_dword_count` and related counters identified a bay with a bad link.
- **ZFS 2.3+ offers JSON output** (`zpool status -j`), which avoids scraping
  the text format.

## Architecture

```
cmd/gdiskmap/        CLI entry point and subcommand dispatch (stdlib flag)
internal/sysfs/      Discovery of controllers, end devices, phys, block devices
internal/mpt/        MPI message encoding and the mpt ctl ioctl
internal/zfs/        zpool status JSON parsing and by-id resolution
internal/inventory/  Merges the sources into one model of slots and disks
internal/layout/     Layout config load/save; calibration pattern inference
internal/output/     Table and JSON rendering
```

### Data model

- **Controller:** IOC number, PCI address, chip, firmware version, host SAS
  address, ctl device path.
- **Enclosure:** logical ID, enclosure handle, start slot, slot count, owning
  controller. Direct-attach HBAs present one virtual enclosure each.
- **Slot:** enclosure, slot number, label (from the layout config, else
  `c<ioc>:s<slot>`), optional disk, LED state, link error counters for the phy
  serving the slot (when a disk is attached and the phy is known).
- **Disk:** block device, device handle, phy, model, serial, WWN, size, ZFS
  membership (pool, vdev path, state).
- **Missing pool member:** a vdev ZFS expects but cannot open (pool, vdev
  GUID, last known path). It cannot be placed in a slot, so it is listed
  separately.

### Discovery (`internal/sysfs`)

Reads through an `fs.FS` rooted at `/sys` (and `/dev/disk/by-id` via a
separate root) so that tests can run against a captured tree on any OS.

1. Enumerate `/sys/class/scsi_host/host*` whose `proc_name` is `mpt2sas` or
   `mpt3sas`; read `unique_id`, `version_fw`, `board_name`,
   `host_sas_address`; derive the PCI address from the device path.
2. Enumerate `end_device-*` under each host; read `bay_identifier`,
   `enclosure_identifier`, `sas_address`; find the HBA phy from the parent
   `sas_port`; find the block device and its `sas_device_handle`, serial
   (`vpd_pg80`), WWN (`wwid`), model and size.
3. Read the phy error counters under `/sys/class/sas_phy`.

### MPI access (`internal/mpt`)

- Opens `/dev/mpt2ctl` (SAS2) or `/dev/mpt3ctl` (SAS3) and issues the
  `MPT2COMMAND`/`MPT3COMMAND` ioctl with the target IOC number.
- **Only two message builders exist:**
  - `CONFIG` with action `PAGE_HEADER` or `READ_CURRENT` (used for Enclosure
    Page 0).
  - `SCSI_ENCLOSURE_PROCESSOR` with action `READ_STATUS` or `WRITE_STATUS`,
    addressed by device handle (occupied slots) or enclosure handle + slot
    (empty slots). Slot status bits used: `IDENTIFY_REQUEST` (0x00020000) and
    `PREDICTED_FAULT` (0x40).

  There is no general passthrough; nothing that writes firmware, NVDATA or
  persistent configuration can be constructed.
- The ioctl carries user-space buffer addresses. Buffers are allocated with
  `mmap` (outside the Go heap) so the garbage collector cannot move them.
- `WRITE_STATUS` replaces the slot's whole status word, so a change to one bit
  first reads the current status and preserves the other bits.

### ZFS (`internal/zfs`)

- Runs `zpool status -j` (requires OpenZFS 2.3 or later; older versions are
  reported as unsupported rather than scraped).
- Resolves each vdev path through `/dev/disk/by-id` to a block device, strips
  the partition, and matches it to a discovered disk.
- Vdevs ZFS cannot open become missing pool members.

### Layout config (`internal/layout`)

TOML at `/etc/gdiskmap.toml` (overridable with `--config`). Keyed by the
enclosure logical ID, which equals the HBA's SAS address and survives moving
the card to another PCI slot.

```toml
[[enclosure]]
id = "0x500605b0071f24b0"
name = "front"            # optional

  [enclosure.slots]
  0 = "11"
  1 = "10"
  2 = "9"
  3 = "8"
```

Slots without an entry use the default label `c<ioc>:s<slot>`.

## Commands

### `gdiskmap disks` (default)

One row per slot across all enclosures, empty slots included. Columns: label,
controller, slot, device, model, serial, size, pool/vdev, ZFS state, LED
state, link errors (non-zero only). A following section lists missing pool
members. Warnings (duplicate slot numbers, unreadable sources) are printed to
stderr. `--json` emits the full model.

### `gdiskmap locate on|off <target>...` / `gdiskmap fault on|off <target>...`

Targets: `sdX`, `/dev/...` (including by-id paths), serial, WWN, bay label,
`c<n>:s<slot>`, `all` (including disks without a known slot), `occupied` or
`empty`. Controllers are numbered `c0`, `c1`, ... in the order SAS2 then
SAS3, each by IOC number, which matches the vendor tools' index on hosts
with one HBA generation. Ambiguous targets are rejected with the
candidates listed. Occupied slots are addressed by device handle; empty slots
by enclosure + slot.

### `gdiskmap calibrate`

An interactive wizard that builds the layout config by lighting slots and
asking which front-panel bay is lit.

- For each enclosure, it lights one slot (identify LED), asks for the bay
  number, then turns the LED off. Input can also be `s` (skip: bay not visible
  or LED not working) or `q` (stop and save progress).
- **Pattern inference:** slots are grouped by connector (4 slots per group by
  default, `--group-size` to override). Within a group, the wizard assumes
  `bay = base ± (slot − first slot)`. After two answers in a group it predicts
  the rest of the group and verifies the pattern by lighting one predicted
  slot and asking for confirmation (`Is this bay 7? [Y/n/number]`). For each
  following group it predicts a base continuing the previous group's
  numbering in the same direction, and verifies that with one blink. In the
  best case an 8-slot controller needs two answers and two confirmations.
  A rejected prediction drops back to asking for every slot in that group.
  `--confirm-all` verifies every predicted slot individually instead.
- A skipped slot is filled in from a confirmed pattern, so empty bays whose
  LEDs do not light still receive a label.
- Bay numbers are integers. Arbitrary text labels can be set by editing the
  config.
- Empty slots are included, because their LEDs can be addressed by enclosure
  + slot. If a backplane does not light empty bays, the user skips them.
- LEDs are always turned off on exit, including on interrupt (SIGINT/SIGTERM).
- Before writing, the resulting mapping is shown and confirmation requested;
  an existing config is backed up rather than overwritten silently. Saving
  replaces all labels of the calibrated enclosures; bay numbers labelled in
  other enclosures cannot be reused.

### `gdiskmap dump <dir>`

Copies the sysfs and `/dev` entries gdiskmap reads (recorded as discovery
runs, symbolic links included), the `zpool status -j` output and the decoded
enclosure lists into a directory. Used for test fixtures and bug reports.
`gdiskmap -root <dir>` replays a capture; LED commands and calibration are
unavailable in that mode.

## Error handling

- **No mpt ctl device or not root:** `disks` still works from sysfs alone,
  with a notice that empty slots and LED state are unavailable. LED commands
  fail with a clear message.
- **No ZFS or ZFS older than 2.3:** pool columns are omitted with a notice,
  so that `-` in the pool column always means "in no pool".
- **No `mpt2sas`/`mpt3sas` hosts:** the tool exits non-zero with a message.
- **An MPI request fails (non-success IOCStatus):** the status code is
  reported with the controller and slot; no retry loops.
- **An MPI request times out:** `mpt3sas` resets the controller. Requests use
  a 10-second timeout and are only sent when needed.
- **Duplicate (enclosure, slot) pairs:** reported as a firmware slot-table
  problem; LEDs for affected disks still work via device handle.

## Testing

- **sysfs and ZFS:** table-driven tests against captured trees from the
  reference host (via `gdiskmap dump`), loaded as `fs.FS`. Fixtures include
  the duplicate-slot state seen before the reflash.
- **MPI encoding:** byte-level tests of each request against the structure
  layouts and constants in the kernel's `mpi2_cnfg.h` and `mpi2_init.h`, and
  parsing tests for captured replies.
- **Calibration inference:** unit tests of the pattern logic with simulated
  answers (forward, reversed per connector, skipped and contradictory input).
- **Hardware:** manual verification on the reference host for LED on/off by
  handle, by empty slot, and through `calibrate`.
- `go vet` and `golangci-lint` on all code.
