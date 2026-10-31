package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ledLockPath is the lock file that serialises LED changes between gdiskmap
// processes. A change is a read-modify-write of the slot status word, so two
// unserialised commands (e.g. `fault on` and `locate on` for the same slot)
// could each overwrite the other's bit. Calibration holds the lock for its
// whole run so that no other command changes the LEDs the user is reading.
// Other tools, such as sas2ircu, do not take this lock.
const ledLockPath = "/run/lock/gdiskmap.lock"

// lockLEDs takes the LED lock, waiting (with a note) if another gdiskmap
// process holds it. The returned function releases it; the lock is also
// released when the process exits.
func lockLEDs(e *env) (unlock func(), err error) {
	f, err := os.OpenFile(ledLockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the LED lock: %w", err)
	}
	fd := int(f.Fd())
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		e.notef("waiting for another gdiskmap command that is using the LEDs (lock %s)", ledLockPath)
		err = unix.Flock(fd, unix.LOCK_EX)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("taking the LED lock %s: %w", ledLockPath, err)
	}
	// Closing the descriptor releases the lock.
	return func() { f.Close() }, nil
}
