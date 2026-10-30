# gdiskmap

gdiskmap shows which physical bay each disk on an LSI SAS HBA sits in, which
bays are empty, and which ZFS pool each disk belongs to. It also turns the
bay identify (locate) and fault LEDs on and off.

It is a single static Linux binary with no dependency on `sas2ircu`,
`sas3ircu` or `storcli`. It is inspired by
[DiskMap](https://github.com/enix/DiskMap), which is Solaris/illumos-only and
depends on `sas2ircu`.

## Requirements

- Linux on amd64 or arm64.
- LSI/Broadcom SAS2 or SAS3 HBAs driven by the `mpt3sas` driver (which also
  handles SAS2 chips such as the SAS2008 and SAS2308), with disks on a
  direct-attach backplane. Expander backplanes are not supported.
- Root, to read empty slots and LED state and to set LEDs. Without root,
  `disks` still lists occupied slots from sysfs.
- OpenZFS 2.3 or later for pool membership (`zpool status -j`). Without it,
  the pool columns are omitted.

## Building

```sh
make build        # bin/gdiskmap-linux-amd64 and bin/gdiskmap-linux-arm64
make test lint    # tests and golangci-lint
```

Copy the binary for your architecture to the host, e.g. to
`/usr/local/sbin/gdiskmap`.

## Usage

```
gdiskmap [flags] [command] [arguments]
```

| Command | Description |
|---|---|
| `disks [-json] [-leds]` | List every slot with its disk, pool membership and link errors. The default command. `-leds` also reads each slot's LED state, which sends one MPI request per slot. |
| `locate on\|off <target>...` | Turn the identify LED on or off. |
| `fault on\|off <target>...` | Turn the fault LED on or off. |
| `calibrate [-group-size N] [-confirm-all] [-light-empty] [controller...]` | Build the bay label config interactively. See [doc/calibration.md](doc/calibration.md). |
| `dump <dir>` | Capture the data gdiskmap reads, for bug reports and offline replay. |
| `help` | Show usage. |

| Flag | Default | Description |
|---|---|---|
| `-config file` | `/etc/gdiskmap.toml` | Bay label config. |
| `-root dir` | | Replay a capture made by `dump` instead of reading the live system. |
| `-zpool command` | `zpool` | The zpool command. |

### Targets

LED commands accept any of:

- `all`, `occupied` or `empty`;
- a bay label from the config, or a slot such as `c0:s3` (controller 0,
  slot 3);
- a device: `sdb`, `/dev/sdb` or `/dev/disk/by-id/...`;
- a serial number (case-insensitive);
- a WWN, with or without its `naa.` or `0x` prefix.

A target that matches more than one slot is rejected.

### Example

Illustrative output, abridged, with bay labels configured:

```
# gdiskmap
BAY  SLOT   DEVICE  MODEL            SERIAL        SIZE  POOL     VDEV      STATE   LINK ERRS
8    c0:s3  sdb     CT2000BX500SSD1  2245E6823C14  2.0T  archive  mirror-2  ONLINE
9    c0:s2  sdd     ST2000NX0433     S460RYB4      2.0T  archive  mirror-3  ONLINE
...
12   c0:s4  -       (empty)          -             -     -        -         -
...

Pool members not found on any device:
POOL   VDEV      NAME                 STATE    LAST PATH
rpool  mirror-0  5587783532978086300  UNAVAIL  /dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3

# gdiskmap locate on empty      # light every empty bay
# gdiskmap fault on S460RYD7    # mark a disk by serial number
# gdiskmap locate off all
```

Notes on the output:

- `BAY` is the label from the config, or the slot (`c<controller>:s<slot>`)
  if none is configured. The `SLOT` column appears only when some slots have
  labels.
- Controllers are numbered by the driver's adapter number, which matches the
  index `sas2ircu`/`sas3ircu` use on a host with only one HBA generation.
- `POOL` is `-` for a disk in no pool.
- `LED` appears only with `-leds`, and only if the controller reports LED
  state.
- `LINK ERRS` is the sum of the HBA phy error counters (invalid dwords,
  disparity errors, loss of dword sync, phy reset problems) for the disk's
  phy. Non-zero values point at a bad cable, backplane slot or connector.
- Pool members whose device does not exist are listed separately, because
  they cannot be placed in a slot.
- Warnings, such as two disks reported in the same slot (a sign of a wrong
  slot table in the HBA's NVDATA), are printed to stderr.

## Bay labels

Slot numbers rarely match the front-panel bay numbers; on common backplanes
the firmware numbers each 4-lane connector in reverse. The optional config
maps slots to labels:

```toml
[[enclosure]]
id = "0x500605b0071f24b0"   # Enclosure logical ID (the HBA's SAS address)
name = "front"              # Optional

  [enclosure.slots]
  0 = "11"
  1 = "10"
```

Labels must be unique, and `all`, `occupied` and `empty` are reserved.
`gdiskmap calibrate` writes this file for you; see
[doc/calibration.md](doc/calibration.md).

## Capturing and replaying

`gdiskmap dump <dir>` copies exactly the sysfs and `/dev` entries gdiskmap
reads, plus the `zpool status -j` output and the controllers' enclosure
lists, into `<dir>`. `gdiskmap -root <dir> disks` then reproduces the output
on any machine. LED commands and calibration are not available with
`-root`.

## How it works

Disk-to-slot mapping comes from sysfs, where `mpt3sas` publishes each
device's slot, enclosure and firmware device handle. Empty slots and LEDs
need the HBA firmware, which gdiskmap reaches through a small, fixed set of
MPI requests sent through `/dev/mpt2ctl` or `/dev/mpt3ctl`. See
[doc/mpi.md](doc/mpi.md) for what is sent and the safety considerations.

## Limitations

- Direct-attach backplanes only. Disks behind SAS expanders are not
  discovered.
- If an HBA's NVDATA carries the wrong slot table, slot numbers are wrong for
  every tool, including this one, and LEDs light the wrong bays. gdiskmap
  warns about duplicate slots. The fix is to flash the NVDATA that matches
  the card.
