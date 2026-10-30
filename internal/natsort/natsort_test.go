package natsort

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCompare(t *testing.T) {
	got := []string{"mirror-10", "mirror-9", "mirror-1", "a", "mirror-01", "b2", "b10", "10", "9", "c0:s1"}
	slices.SortStableFunc(got, Compare)
	want := []string{"9", "10", "a", "b2", "b10", "c0:s1", "mirror-1", "mirror-01", "mirror-9", "mirror-10"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sort with Compare mismatch (-want +got):\n%s", diff)
	}
}
