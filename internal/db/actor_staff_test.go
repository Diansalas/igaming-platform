package db

// ADR 0119 S4b section 9.5: the pure re-assertion rule, and no app.actor_staff_id setter yet.

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var actorStaffGUCPattern = regexp.MustCompile(`set_config\(\s*'app\.actor_staff_id'`)

// ADR 0119 S4b: the app.actor_staff_id GUC of section 9.5(b) is NOT built (no flow re-asserted in S4b needs it).
// Nothing may set it until the slice that adds the staff_actor_self_* policy pair and its single setter.
func TestStatic_NoActorStaffIDSetterYet(t *testing.T) {
	root := repoRootForActingSetterTest(t)
	var offenders []string
	scanned := 0
	if err := walkGoFilesSkippingHiddenAndTestdata(root, func(path string, src []byte) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		if actorStaffGUCPattern.Match(src) {
			offenders = append(offenders, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("vacuous scan")
	}
	if len(offenders) > 0 {
		t.Fatalf("app.actor_staff_id set outside its (future) single setter: %v", offenders)
	}
	if !actorStaffGUCPattern.MatchString(`tx.Exec(ctx, "SELECT set_config('app.actor_staff_id', $1, true)")`) {
		t.Fatal("negative control: the planted setter was not detected")
	}
}

func TestCheckActorStaffLive(t *testing.T) {
	actor, other := uuid.New(), uuid.New()
	rows := []ActorStaffRow{{ID: other, Role: "finance", Status: "active"}, {ID: actor, Role: "finance", Status: "active"}}
	if err := CheckActorStaffLive(rows, actor, "finance"); err != nil {
		t.Fatalf("live actor: %v", err)
	}
	for name, c := range map[string]struct {
		rows  []ActorStaffRow
		actor uuid.UUID
		role  string
	}{
		"absent":         {rows[:1], actor, "finance"},
		"suspended":      {[]ActorStaffRow{{ID: actor, Role: "finance", Status: "suspended"}}, actor, "finance"},
		"role changed":   {rows, actor, "support"},
		"empty role":     {rows, actor, ""},
		"nil actor":      {rows, uuid.Nil, "finance"},
		"only the other": {rows, uuid.New(), "finance"},
	} {
		if err := CheckActorStaffLive(c.rows, c.actor, c.role); !errors.Is(err, ErrActorStaffNotLive) {
			t.Errorf("%s: want ErrActorStaffNotLive, got %v", name, err)
		}
	}
}
