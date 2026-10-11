package audit

import (
	"strings"
	"testing"
)

// ADR 0112 slice 4: the launch authorisation events are tenant-presented with a reason code (and
// before/after for the status-changing ones) and nothing more; refusal rows have no entry.
func TestTenantPresentation_LaunchAuthorisationEntries(t *testing.T) {
	events := map[string]TenantPresentationEntry{
		"launch_authorisation.requested":            {ReasonCode: true},
		"launch_authorisation.approved":             {ReasonCode: true},
		"launch_authorisation.rejected":             {ReasonCode: true},
		"launch_authorisation.cancelled":            {ReasonCode: true},
		"launch_authorisation.expired":              {ReasonCode: true},
		"launch_authorisation.superseded":           {ReasonCode: true},
		"launch_authorisation.refused_at_execution": {ReasonCode: true},
		"launch_authorisation.executed":             {ReasonCode: true, BeforeAfter: true},
		"launch_authorisation.suspended":            {ReasonCode: true, BeforeAfter: true},
		"launch_authorisation.ratified":             {ReasonCode: true, BeforeAfter: true},
	}
	for action, want := range events {
		if got, ok := TenantPresentation[action]; !ok || got != want {
			t.Errorf("%s: entry %+v (present=%v), want %+v", action, got, ok, want)
		}
	}
	launch := 0
	for action, e := range TenantPresentation {
		if !strings.HasPrefix(action, "launch_authorisation.") {
			continue
		}
		launch++
		if strings.HasSuffix(action, "_denied") {
			t.Errorf("%s: a refusal row must not be presented beyond the base columns", action)
		}
		if _, ok := events[action]; !ok {
			t.Errorf("%s: unexpected launch presentation entry %+v", action, e)
		}
	}
	if launch != len(events) {
		t.Errorf("launch entries: %d, want %d", launch, len(events))
	}
}
