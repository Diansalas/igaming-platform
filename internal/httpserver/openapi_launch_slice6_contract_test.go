package httpserver

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// ADR 0112 slice 6, DB-free: the new fields, the closed can_approve reason catalogue, the queue
// paging contract and the list-row launch_kind are in the spec exactly as the code produces them.

func enumOf(t *testing.T, block, prop string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\n\s+` + prop + `:\n.*?enum: \[([^\]]*)\]`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("no enum for %s", prop)
	}
	var out []string
	for _, v := range strings.Split(m[1], ",") {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

func TestOpenAPI_LaunchSlice6_CannotApproveReasonEnumIsTheGoCatalogue(t *testing.T) {
	spec := launchSpec(t)
	got := enumOf(t, schemaBlock(t, spec, "LaunchRequest"), "cannot_approve_reason")
	want := append([]string(nil), launchgov.CannotApproveReasons...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cannot_approve_reason enum %v != launchgov.CannotApproveReasons %v", got, want)
	}
	if len(want) != 8 {
		t.Fatalf("the catalogue is closed at 8 reasons; a change needs this test, the spec and the UI together (got %d)", len(want))
	}
}

func TestOpenAPI_LaunchSlice6_QueuePagingContract(t *testing.T) {
	spec := launchSpec(t)
	b := openAPIPathBlock(t, spec, "/v1/admin/launch-authorisations")
	for _, want := range []string{"maximum: 200", "default: 50", "name: cursor", "LaunchQueueList", "(created_at, id) DESCENDING", "never the name of another staff member"} {
		if !strings.Contains(b, want) {
			t.Errorf("queue entry lacks %q", want)
		}
	}
	if strings.Contains(b, "maximum: 500") {
		t.Error("the queue limit is capped at 200")
	}
	q := schemaBlock(t, spec, "LaunchQueueList")
	if !regexp.MustCompile(`required: \[disclaimer, items, next_cursor\]`).MatchString(q) || !strings.Contains(q, "nullable: true") {
		t.Error("LaunchQueueList must require next_cursor and make it nullable")
	}
	if launchgov.QueueDefaultLimit != 50 || launchgov.QueueMaxLimit != 200 {
		t.Errorf("queue bounds drifted from the spec: %d/%d", launchgov.QueueDefaultLimit, launchgov.QueueMaxLimit)
	}
}

func TestOpenAPI_LaunchSlice6_ListRowsCarryLaunchKind(t *testing.T) {
	spec := launchSpec(t)
	for name, dto := range map[string]any{"Tenant": tenantListRow{}, "Brand": brandListRow{}} {
		props := launchSchemaProps(schemaBlock(t, spec, name))
		if !props["launch_kind"] {
			t.Errorf("%s schema lacks launch_kind", name)
		}
		fields := jsonFields(reflect.TypeOf(dto))
		seen := map[string]bool{}
		for _, f := range fields {
			seen[f] = true
			if !props[f] {
				t.Errorf("%s: wire field %q not in schema", name, f)
			}
		}
		for p := range props {
			if !seen[p] {
				t.Errorf("%s: schema property %q not produced by the list row", name, p)
			}
		}
		if r := regexp.MustCompile(`required: \[[^\]]*launch_kind`).MatchString(schemaBlock(t, spec, name)); r {
			t.Errorf("%s: launch_kind is optional (null on detail/create), not required", name)
		}
	}
}

// The DTO carries the caller-specific fields as optional (absent on write responses).
func TestLaunchRequestDTO_Slice6FieldsAreOptionalOnTheWire(t *testing.T) {
	for _, f := range []string{"tenant_name", "brand_name", "can_approve", "cannot_approve_reason"} {
		found := false
		typ := reflect.TypeOf(launchRequestDTO{})
		for i := 0; i < typ.NumField(); i++ {
			if tag := typ.Field(i).Tag.Get("json"); strings.HasPrefix(tag, f+",") && strings.Contains(tag, "omitempty") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must be an omitempty field of launchRequestDTO", f)
		}
	}
}
