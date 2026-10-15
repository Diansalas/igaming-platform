package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ADR 0114 H1 on the AML case detail: a tenant-scoped viewer never sees the staff id of a PLATFORM actor (only the scope marker); it
// keeps seeing its own tenant's staff ids; a platform viewer sees everything. Notes follow the case's owner scope (the database lets
// only the owner scope write to a case).
type redactFake struct {
	fakeAML
	owner                              string
	tenantActor, platformActor, noteBy uuid.UUID
	noteID                             uuid.UUID
	noteScope                          string // the actor_scope of the event that recorded the note ("" = no such event)
}

func (f *redactFake) ReadCase(_ context.Context, _ aml.Actor, id uuid.UUID) (aml.Case, error) {
	return aml.Case{ID: id, OwnerScope: f.owner, PersonID: uuid.New(), Kind: aml.CaseKindManual, Status: aml.StatusInReview, LastSeq: 3, OpenedAt: time.Unix(0, 0)}, nil
}
func (f *redactFake) CaseEvents(context.Context, aml.Actor, uuid.UUID) ([]aml.CaseEvent, error) {
	evs := []aml.CaseEvent{
		{Seq: 1, Type: "opened", ReasonCode: "r", ActorID: f.tenantActor, ActorScope: aml.ScopeTenant, CreatedAt: time.Unix(0, 0)},
		{Seq: 2, Type: "status_changed", ReasonCode: "r", ActorID: f.platformActor, ActorScope: aml.ScopePlatformActing, CreatedAt: time.Unix(0, 0)},
		{Seq: 3, Type: "note_added", ReasonCode: "r", ActorID: f.platformActor, ActorScope: "something_new", CreatedAt: time.Unix(0, 0)},
	}
	if f.noteScope != "" {
		evs = append(evs, aml.CaseEvent{Seq: 4, Type: "note_added", NoteID: &f.noteID, ReasonCode: "r", ActorID: f.noteBy, ActorScope: f.noteScope, CreatedAt: time.Unix(0, 0)})
	}
	return evs, nil
}
func (f *redactFake) CaseNotes(context.Context, aml.Actor, uuid.UUID) ([]aml.CaseNote, error) {
	return []aml.CaseNote{{ID: f.noteID, Body: "b", LintVersion: "v", CreatedBy: f.noteBy, CreatedAt: time.Unix(0, 0)}}, nil
}

func TestAMLCaseDetail_StaffIdentityRedaction(t *testing.T) {
	tid := uuid.New()
	run := func(f *redactFake, tc tenant.Context) (events []map[string]any, notes []map[string]any) {
		t.Helper()
		mux := amlTestMux(f, tc)
		rec := amlDo(mux, "GET", "/t/"+tid.String()+"/cases/"+uuid.NewString(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var out struct {
			Events []map[string]any `json:"events"`
			Notes  []map[string]any `json:"notes"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Events, out.Notes
	}
	tenantStaff, platformStaff, me := uuid.New(), uuid.New(), uuid.New()
	tenantViewer := tenant.Context{TenantID: tid, Subject: me.String(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)}
	platformViewer := tenant.Context{TenantID: uuid.Nil, Subject: me.String(), Role: string(auth.RolePlatformAdmin), PrincipalType: string(auth.PrincipalStaff)}

	// A tenant viewer of a tenant-owned case: own tenant ids visible, a platform actor / unknown scope hidden (null).
	f := &redactFake{owner: "tenant", tenantActor: tenantStaff, platformActor: platformStaff, noteBy: tenantStaff, noteID: uuid.New(), noteScope: aml.ScopeTenant}
	ev, nt := run(f, tenantViewer)
	if ev[0]["actor_id"] != tenantStaff.String() || ev[1]["actor_id"] != nil || ev[2]["actor_id"] != nil || ev[3]["actor_id"] != tenantStaff.String() {
		t.Errorf("tenant viewer events: %v", ev)
	}
	if nt[0]["created_by"] != tenantStaff.String() {
		t.Errorf("tenant viewer, note recorded by a tenant actor: %v", nt)
	}
	// A note recorded by a PLATFORM actor and a note with an unknown scope are hidden, and the id appears nowhere in the body.
	for name, scope := range map[string]string{"platform actor": aml.ScopePlatformActing, "unknown scope": "something_new"} {
		g := &redactFake{owner: "tenant", tenantActor: tenantStaff, platformActor: platformStaff, noteBy: platformStaff, noteID: uuid.New(), noteScope: scope}
		_, nt = run(g, tenantViewer)
		if nt[0]["created_by"] != nil {
			t.Errorf("tenant viewer, %s note must be hidden: %v", name, nt)
		}
		rec := amlDo(amlTestMux(g, tenantViewer), "GET", "/t/"+tid.String()+"/cases/"+uuid.NewString(), "")
		if strings.Contains(rec.Body.String(), platformStaff.String()) {
			t.Errorf("%s: a platform staff id reached a tenant viewer: %s", name, rec.Body)
		}
	}
	// A note with NO recording event has an unknown scope and is hidden.
	h := &redactFake{owner: "tenant", tenantActor: tenantStaff, platformActor: platformStaff, noteBy: platformStaff, noteID: uuid.New()}
	if _, nt = run(h, tenantViewer); nt[0]["created_by"] != nil {
		t.Errorf("a note with no recording event is hidden: %v", nt)
	}
	// A platform viewer sees everything.
	g := &redactFake{owner: "platform", tenantActor: tenantStaff, platformActor: platformStaff, noteBy: platformStaff, noteID: uuid.New(), noteScope: aml.ScopePlatformActing}
	ev, nt = run(g, platformViewer)
	if ev[1]["actor_id"] != platformStaff.String() || ev[2]["actor_id"] != platformStaff.String() || nt[0]["created_by"] != platformStaff.String() {
		t.Errorf("platform viewer must see every id: %v %v", ev, nt)
	}
}
