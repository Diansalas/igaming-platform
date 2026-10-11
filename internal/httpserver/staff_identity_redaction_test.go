package httpserver

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func TestStaffViewer_Table(t *testing.T) {
	id := uuid.New()
	platform := staffViewerOf(tenant.Context{TenantID: uuid.Nil})
	tenantV := staffViewerOf(tenant.Context{TenantID: uuid.New()})
	cases := []struct {
		name    string
		v       staffViewer
		scope   string
		visible bool
	}{
		{"platform viewer sees platform actor", platform, "platform", true},
		{"platform viewer sees tenant actor", platform, "tenant", true},
		{"tenant viewer sees tenant actor", tenantV, "tenant", true},
		{"tenant viewer must not see platform actor", tenantV, "platform", false},
		{"tenant viewer must not see empty scope (fail closed)", tenantV, "", false},
		{"tenant viewer must not see unknown scope (fail closed)", tenantV, "Tenant", false},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v.ref(c.scope, id))
		if err != nil {
			t.Fatal(err)
		}
		want := "null"
		if c.visible {
			want = `"` + id.String() + `"`
		}
		if string(b) != want {
			t.Errorf("%s: got %s, want %s", c.name, b, want)
		}
	}
}

func TestStaffRef_ZeroValueAndNilUUIDAreNull(t *testing.T) {
	if b, _ := json.Marshal(staffRef{}); string(b) != "null" {
		t.Fatalf("zero staffRef = %s, want null (fail closed)", b)
	}
	if b, _ := json.Marshal(staffViewer{platform: true}.ref("platform", uuid.Nil)); string(b) != "null" {
		t.Fatalf("nil uuid = %s, want null", b)
	}
}

func TestStaffViewer_OptRef(t *testing.T) {
	tenantV := staffViewerOf(tenant.Context{TenantID: uuid.New()})
	if tenantV.optRef(nil, nil) != nil {
		t.Fatal("absent actor must stay nil so an omitempty field is omitted")
	}
	id := uuid.New()
	plat, ten := "platform", "tenant"
	if r := tenantV.optRef(&plat, &id); r == nil || r.id != "" {
		t.Fatalf("platform approver must be a non-nil hidden ref for a tenant viewer, got %+v", r)
	}
	if r := tenantV.optRef(&ten, &id); r == nil || r.id != id.String() {
		t.Fatalf("tenant approver must be visible to a tenant viewer, got %+v", r)
	}
	if r := tenantV.optRef(nil, &id); r == nil || r.id != "" {
		t.Fatalf("approver of unknown scope must be hidden (fail closed), got %+v", r)
	}
	type wrap struct {
		A *staffRef `json:"a,omitempty"`
	}
	if b, _ := json.Marshal(wrap{A: tenantV.optRef(&plat, &id)}); string(b) != `{"a":null}` {
		t.Fatalf("hidden optional ref must render null (present but redacted), got %s", b)
	}
	if b, _ := json.Marshal(wrap{}); string(b) != `{}` {
		t.Fatalf("absent optional ref must be omitted, got %s", b)
	}
}
