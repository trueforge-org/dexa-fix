package models

import "testing"

func TestJobActiveIsTrueOnlyForJobsThatCanStillRun(t *testing.T) {
	for state, want := range map[string]bool{
		JobQueued: true, JobRunning: true,
		JobSucceeded: false, JobFailed: false, JobCanceled: false, JobExpired: false, "": false, "unknown": false,
	} {
		if got := JobActive(state); got != want {
			t.Errorf("JobActive(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestOnlyTheAdminRoleIsAnAdministrator(t *testing.T) {
	for role, want := range map[string]bool{RoleAdmin: true, RoleUser: false, "": false} {
		if got := (User{Role: role}).IsAdmin(); got != want {
			t.Errorf("role %q: IsAdmin = %v, want %v", role, got, want)
		}
	}
}

func TestAUsersLocationFallsBackToUTC(t *testing.T) {
	for _, tc := range []struct {
		zone string
		want string
	}{
		{"", "UTC"},
		{"Europe/Amsterdam", "Europe/Amsterdam"},
		{"Mars/Olympus", "UTC"},
	} {
		if got := (User{Timezone: tc.zone}).Location().String(); got != tc.want {
			t.Errorf("zone %q: Location = %q, want %q", tc.zone, got, tc.want)
		}
	}
}
