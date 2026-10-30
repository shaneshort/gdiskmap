// Command gdiskmap maps disks on LSI SAS HBAs to their physical slots, shows
// their ZFS pool membership, and controls the slot LEDs.
//
// Usage:
//
//	gdiskmap [flags] [command] [arguments]
//
// Run `gdiskmap help` for the list of commands.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"gdiskmap/internal/layout"
	"gdiskmap/internal/mpt"
)

const usageText = `Usage: gdiskmap [flags] [command] [arguments]

Commands:
  disks [-json] [-leds]           List every slot with its disk, pool membership
                                  and link errors (the default command). -leds also
                                  reads each slot's LED state from the controller.
  locate on|off <target>...       Turn the identify (locate) LED on or off.
  fault on|off <target>...        Turn the fault LED on or off.
  calibrate [-group-size N] [-confirm-all] [-light-empty] [controller...]
                                  Build the bay label config by lighting slots and
                                  asking which bay is lit.
  dump <dir>                      Capture the data gdiskmap reads into <dir>, for
                                  bug reports and offline replay with -root.
  help                            Show this help.

Targets: all, occupied, empty, a bay label, a slot (c0:s3), a device (sdb,
/dev/sdb, /dev/disk/by-id/...), a serial number or a WWN.

Flags:
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// usageError reports invalid command-line usage; it exits with status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// run executes gdiskmap with the given arguments and returns the exit
// status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("gdiskmap", flag.ContinueOnError)
	fl.SetOutput(stderr)
	e := &env{stdin: bufio.NewReader(stdin), stdout: stdout, stderr: stderr}
	fl.StringVar(&e.configPath, "config", layout.DefaultPath, "bay label config `file`")
	fl.StringVar(&e.root, "root", "", "replay a capture made by `gdiskmap dump` from `dir` instead of reading the live system")
	fl.StringVar(&e.zpool, "zpool", "zpool", "zpool `command` used to read pool status")
	fl.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cmd, rest := "disks", fl.Args()
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	var err error
	switch cmd {
	case "disks":
		err = cmdDisks(e, rest)
	case "locate":
		err = cmdLED(e, rest, "locate", mpt.StatusIdentify)
	case "fault":
		err = cmdLED(e, rest, "fault", mpt.StatusPredictedFault)
	case "calibrate":
		err = cmdCalibrate(e, rest)
	case "dump":
		err = cmdDump(e, rest)
	case "help":
		fl.Usage()
		return 0
	default:
		err = usageError{fmt.Sprintf("unknown command %q", cmd)}
	}

	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "gdiskmap: %v\nRun 'gdiskmap help' for usage.\n", err)
		return 2
	default:
		fmt.Fprintf(stderr, "gdiskmap: %v\n", err)
		return 1
	}
}
