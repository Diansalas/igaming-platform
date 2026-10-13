package db

// ADR 0119 S4b section 9.5(b) / 13.5: SetActorStaffIDForCurrentTx is the ONLY setter of app.actor_staff_id
// (exactly one setter, pinned), and the pure re-assertion rule.

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var actorStaffGUCPattern = regexp.MustCompile(`set_config\(\s*'app\.actor_staff_id'`)

func TestStatic_SetActorStaffIDIsTheOnlySetter(t *testing.T) {
	root := repoRootForActingSetterTest(t)
	allowed := filepath.Join(root, "internal", "db", "actor_staff.go")
	var offenders []string
	setterFiles := 0
	if err := walkGoFilesSkippingHiddenAndTestdata(root, func(path string, src []byte) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !actorStaffGUCPattern.Match(src) {
			return nil
		}
		if path == allowed {
			setterFiles++
			return nil
		}
		offenders = append(offenders, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if setterFiles != 1 {
		t.Fatalf("the setter file must name app.actor_staff_id (found %d)", setterFiles)
	}
	if len(offenders) > 0 {
		t.Fatalf("app.actor_staff_id referenced outside internal/db/actor_staff.go: %v", offenders)
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
