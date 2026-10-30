// Package natsort compares strings in natural order, so that "mirror-10"
// sorts after "mirror-9" and bay "10" after bay "9".
package natsort

import (
	"cmp"
	"strings"
)

// Compare compares a and b, treating runs of ASCII digits as numbers. It
// returns -1, 0 or +1 like strings.Compare. Digit runs that differ only in
// leading zeros compare equal.
func Compare(a, b string) int {
	for a != "" && b != "" {
		da, db := digitPrefix(a), digitPrefix(b)
		if da != "" && db != "" {
			// Compare digit runs by length (after stripping leading zeros),
			// then lexically, which orders them numerically without
			// overflow.
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if c := cmp.Compare(len(na), len(nb)); c != 0 {
				return c
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return cmp.Compare(a[0], b[0])
		}
		a, b = a[1:], b[1:]
	}
	return cmp.Compare(len(a), len(b))
}

func digitPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
