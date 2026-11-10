//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPreflightExplainsPSICausesWithoutEnablingGuards(t *testing.T) {
	c := defaultConfig()
	cases := []struct {
		name, host, cmd, want string
		targetErr, hostErr    error
	}{
		{"permission", "", "", "io_psi_permission_denied", os.ErrPermission, os.ErrNotExist},
		{"invalid", "", "", "io_psi_invalid", nil, os.ErrNotExist},
		{"cgroup_only", "some avg10=0.00", "", "cgroup_io_psi_missing", os.ErrNotExist, nil},
		{"boot_disabled", "", "quiet psi=0", "psi_disabled_boot_argument", os.ErrNotExist, os.ErrNotExist},
		{"host_invisible", "", "quiet", "psi_host_interface_unavailable", os.ErrNotExist, os.ErrNotExist},
		{"host_permission", "", "", "host_psi_visibility_denied", os.ErrNotExist, os.ErrPermission},
		{"read_error", "", "", "io_psi_read_failed", errors.New("IO error"), os.ErrNotExist},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := Metrics{Time: time.Now(), Version: 2, Cgroup: "/test"}
			read := func(p string) ([]byte, error) {
				switch p {
				case "/test/io.pressure":
					return []byte("bad"), tt.targetErr
				case "/proc/pressure/io":
					return []byte(tt.host), tt.hostErr
				case "/proc/cmdline":
					return []byte(tt.cmd), nil
				}
				t.Fatalf("unexpected read %s", p)
				return nil, nil
			}
			result := preflight(c, m, nil, read)
			found := false
			for _, note := range result.Checks {
				if note.Code == tt.want {
					found = true
				}
			}
			if !found || result.ProtectionReady || c.MaxIOPSIAvg10 != 10 {
				t.Fatalf("%s: %+v", tt.want, result)
			}
		})
	}
}
func TestPreflightDoesNotProbeAfterCgroupResolutionFailure(t *testing.T) {
	for _, err := range []error{os.ErrNotExist, os.ErrPermission, errors.New("not a cgroup")} {
		result := preflight(defaultConfig(), Metrics{}, err, func(string) ([]byte, error) { t.Fatal("unexpected IO after resolution failure"); return nil, nil })
		if result.ProtectionReady || len(result.Checks) != 1 || !strings.HasPrefix(result.Checks[0].Code, "cgroup_") {
			t.Fatal(result)
		}
	}
}

func TestRC12RoleTemplatesKeepStrictGuardsAndIndependentState(t *testing.T) {
	states := map[string]bool{}
	for _, role := range []string{"dbs", "hisdbs", "dbsbackup", "hisdbsbackup"} {
		c, err := loadConfig("deploy/rc12/" + role + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err = c.validate(); err != nil {
			t.Fatal(role, err)
		}
		if c.Action != "observe" || c.MaxIOPSIAvg10 != 10 || c.CriticalMode != "pause" || c.LogKeepDays != 30 || c.LogMaxTotalBytes != 1000000000 || states[c.StateFile] {
			t.Fatal(role, c)
		}
		states[c.StateFile] = true
		if len(c.ExcludeNames) == 0 {
			t.Fatal("critical PG exclusions removed")
		}
	}
}
