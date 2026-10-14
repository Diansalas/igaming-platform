package db

// ADR 0119 section 9.5: the pure re-assertion rule (role, status and, since
// migration 0139, staff_version), and the single setter of app.actor_staff_id.

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var actorStaffGUCPattern = regexp.MustCompile(`set_config\(\s*'app\.actor_staff_id'`)

// ADR 0119 S5 + S6 (section 9.5(b), section 13.5 "exactly one setter"): app.actor_staff_id is set ONLY by
// SetActorStaffIDForCurrentTx in internal/db/actor_staff.go. (S4b pinned that nothing set it; this test replaces
// TestStatic_NoActorStaffIDSetterYet.)
func TestStatic_ActorStaffIDSingleSetter(t *testing.T) {
	root := repoRootForActingSetterTest(t)
	var hits []string
	scanned := 0
	if err := walkGoFilesSkippingHiddenAndTestdata(root, func(path string, src []byte) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		if n := len(actorStaffGUCPattern.FindAllIndex(src, -1)); n > 0 {
			rel, _ := filepath.Rel(root, path)
			for i := 0; i < n; i++ {
				hits = append(hits, filepath.ToSlash(rel))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("vacuous scan")
	}
	if len(hits) != 1 || hits[0] != "internal/db/actor_staff.go" {
		t.Fatalf("app.actor_staff_id must be set exactly once, in internal/db/actor_staff.go; found %v", hits)
	}
	if !actorStaffGUCPattern.MatchString(`tx.Exec(ctx, "SELECT set_config('app.actor_staff_id', $1, true)")`) {
		t.Fatal("negative control: the planted setter was not detected")
	}
}

func TestCheckActorStaffLive(t *testing.T) {
	actor, other := uuid.New(), uuid.New()
	rows := []ActorStaffRow{{ID: other, Role: "finance", Status: "active", StaffVersion: 1}, {ID: actor, Role: "finance", Status: "active", StaffVersion: 3}}
	want := ActorExpectation{Role: "finance", StaffVersion: 3}
	if err := CheckActorStaffLive(rows, actor, want); err != nil {
		t.Fatalf("live actor: %v", err)
	}
	for name, c := range map[string]struct {
		rows  []ActorStaffRow
		actor uuid.UUID
		want  ActorExpectation
	}{
		"absent":         {rows[:1], actor, want},
		"suspended":      {[]ActorStaffRow{{ID: actor, Role: "finance", Status: "suspended", StaffVersion: 3}}, actor, want},
		"role changed":   {rows, actor, ActorExpectation{Role: "support", StaffVersion: 3}},
		"sv older":       {rows, actor, ActorExpectation{Role: "finance", StaffVersion: 2}},
		"sv newer":       {rows, actor, ActorExpectation{Role: "finance", StaffVersion: 4}},
		"sv unversioned": {[]ActorStaffRow{{ID: actor, Role: "finance", Status: "active", StaffVersion: 0}}, actor, ActorExpectation{Role: "finance"}},
		"empty role":     {rows, actor, ActorExpectation{StaffVersion: 3}},
		"nil actor":      {rows, uuid.Nil, want},
		"only the other": {rows, uuid.New(), want},
	} {
		if err := CheckActorStaffLive(c.rows, c.actor, c.want); !errors.Is(err, ErrActorStaffNotLive) {
			t.Errorf("%s: want ErrActorStaffNotLive, got %v", name, err)
		}
	}
}
