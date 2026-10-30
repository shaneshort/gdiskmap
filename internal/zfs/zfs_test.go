package zfs

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// statusSample follows the structure of `zpool status -j` output shown in
// zpool-status(8), populated with the reference host's degraded rpool.
const statusSample = `{
  "output_version": {"command": "zpool status", "vers_major": 0, "vers_minor": 1},
  "pools": {
    "rpool": {
      "name": "rpool",
      "state": "DEGRADED",
      "vdevs": {
        "rpool": {
          "name": "rpool",
          "vdev_type": "root",
          "state": "DEGRADED",
          "vdevs": {
            "mirror-0": {
              "name": "mirror-0",
              "vdev_type": "mirror",
              "state": "DEGRADED",
              "vdevs": {
                "ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3": {
                  "name": "ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3",
                  "vdev_type": "disk",
                  "guid": "1111",
                  "path": "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3",
                  "state": "ONLINE"
                },
                "5587783532978086300": {
                  "name": "5587783532978086300",
                  "vdev_type": "disk",
                  "guid": "5587783532978086300",
                  "path": "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3",
                  "state": "UNAVAIL"
                }
              }
            }
          }
        }
      }
    },
    "archive": {
      "name": "archive",
      "state": "ONLINE",
      "vdevs": {
        "archive": {
          "name": "archive",
          "vdevs": {
            "mirror-10": {
              "name": "mirror-10",
              "vdev_type": "mirror",
              "vdevs": {
                "scsi-35000c5009804f337": {"name": "scsi-35000c5009804f337", "vdev_type": "disk", "guid": "2", "path": "/dev/disk/by-id/scsi-35000c5009804f337", "state": "ONLINE"}
              }
            },
            "mirror-9": {
              "name": "mirror-9",
              "vdev_type": "mirror",
              "vdevs": {
                "scsi-35000c5009804fa5f": {"name": "scsi-35000c5009804fa5f", "vdev_type": "disk", "guid": "3", "path": "/dev/disk/by-id/scsi-35000c5009804fa5f", "state": "ONLINE"}
              }
            }
          }
        }
      },
      "l2cache": {
        "nvme0n1": {"name": "nvme0n1", "vdev_type": "disk", "guid": "4", "path": "/dev/nvme0n1", "state": "ONLINE"}
      },
      "spares": {
        "sdz": {"name": "sdz", "vdev_type": "disk", "guid": "5", "path": "/dev/sdz", "state": "AVAIL"}
      }
    }
  }
}`

func TestParse(t *testing.T) {
	got, err := Parse([]byte(statusSample))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	want := []Vdev{
		{Pool: "archive", Parent: "mirror-9", Name: "scsi-35000c5009804fa5f", GUID: "3", Path: "/dev/disk/by-id/scsi-35000c5009804fa5f", State: "ONLINE"},
		{Pool: "archive", Parent: "mirror-10", Name: "scsi-35000c5009804f337", GUID: "2", Path: "/dev/disk/by-id/scsi-35000c5009804f337", State: "ONLINE"},
		{Pool: "archive", Class: "cache", Name: "nvme0n1", GUID: "4", Path: "/dev/nvme0n1", State: "ONLINE"},
		{Pool: "archive", Class: "spare", Name: "sdz", GUID: "5", Path: "/dev/sdz", State: "AVAIL"},
		{Pool: "rpool", Parent: "mirror-0", Name: "5587783532978086300", GUID: "5587783532978086300", Path: "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400D2240AGN-part3", State: "UNAVAIL"},
		{Pool: "rpool", Parent: "mirror-0", Name: "ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3", GUID: "1111", Path: "/dev/disk/by-id/ata-INTEL_SSDSC2KG240G8_PHYG006400DA240AGN-part3", State: "ONLINE"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Parse() mismatch (-want +got):\n%s", diff)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "not json", `{"pools": {"p": {"vdevs": []}}}`} {
		if got, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) = %v, want error", in, got)
		}
	}
}

func TestLocation(t *testing.T) {
	tests := []struct {
		v    Vdev
		want string
	}{
		{Vdev{Parent: "mirror-0"}, "mirror-0"},
		{Vdev{Class: "cache"}, "cache"},
		{Vdev{Class: "log", Parent: "mirror-1"}, "log/mirror-1"},
		{Vdev{}, ""},
	}
	for _, tc := range tests {
		if got := tc.v.Location(); got != tc.want {
			t.Errorf("%+v.Location() = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestNaturalCompare(t *testing.T) {
	got := []string{"mirror-10", "mirror-9", "mirror-1", "a", "mirror-01", "b2", "b10"}
	slices.SortStableFunc(got, naturalCompare)
	want := []string{"a", "b2", "b10", "mirror-1", "mirror-01", "mirror-9", "mirror-10"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sort with naturalCompare mismatch (-want +got):\n%s", diff)
	}
}
