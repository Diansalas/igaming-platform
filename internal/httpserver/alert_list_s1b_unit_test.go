package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ADR 0114 S1b, DB-free: the cursor codec and the staff-identity routing of the alert DTO.

func TestAlertCursor_RoundTripKeepsMicrosecondPrecision(t *testing.T) {
	id := uuid.New()
	ts := time.Date(2026, 10, 14, 12, 30, 45, 123456000, time.UTC) // exactly microsecond
	c, err := decodeAlertCursor(encodeAlertCursor(ts, id.String(), "open|p1|"), "open|p1|")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != id || c.Micros != ts.UnixMicro() {
		t.Fatalf("round trip: %+v", c)
	}
}

func TestAlertCursor_RefusesWrongFilterAndBounds(t *testing.T) {
	id := uuid.NewString()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	good := encodeAlertCursor(ts, id, "a|b|c")
	if _, err := decodeAlertCursor(good, "a|b|d"); err == nil {
		t.Error("a cursor must be bound to its filter")
	}
	for _, bad := range []time.Time{time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2200, 1, 1, 0, 0, 1, 0, time.UTC)} {
		if _, err := decodeAlertCursor(encodeAlertCursor(bad, id, "f"), "f"); err == nil {
			t.Errorf("time %v must be refused", bad)
		}
	}
	// the inclusive bounds are accepted
	for _, edge := range []int64{alertCursorMinMicros, alertCursorMaxMicros} {
		if _, err := decodeAlertCursor(encodeAlertCursor(time.UnixMicro(edge), id, "f"), "f"); err != nil {
			t.Errorf("edge %d must be accepted: %v", edge, err)
		}
	}
	if _, err := decodeAlertCursor(encodeAlertCursor(ts, id, "f")+strings.Repeat("A", alertCursorMaxLen), "f"); err == nil {
		t.Error("oversized cursor must be refused")
	}
	if _, err := decodeAlertCursor("", "f"); err == nil {
		t.Error("empty cursor must be refused")
	}
	if _, err := decodeAlertCursor(encodeAlertCursor(ts, uuid.Nil.String(), "f"), "f"); err != nil {
		t.Errorf("a syntactically valid id is a valid position: %v", err)
	}
}

func TestAlertActorRefs_RoutedThroughTheStaffViewer(t *testing.T) {
	acker, resolver := uuid.New(), uuid.New()
	render := func(v staffViewer, a, r *uuid.UUID) string {
		ak, rs := alertActorRefs(v, a, r)
		raw, err := json.Marshal(alertListItemDTO{AckedBy: ak, ResolvedBy: rs})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	// Platform viewer: ids shown.
	pv := staffViewerOf(tenant.Context{TenantID: uuid.Nil})
	out := render(pv, &acker, &resolver)
	if !strings.Contains(out, acker.String()) || !strings.Contains(out, resolver.String()) {
		t.Errorf("platform viewer must see the actors: %s", out)
	}
	// Absent actors are omitted, not null.
	if out := render(pv, nil, nil); strings.Contains(out, "acked_by") || strings.Contains(out, "resolved_by") {
		t.Errorf("absent actors must be omitted: %s", out)
	}
	// Tenant viewer (never reachable through the routes today): ids never in the body, field is null.
	tv := staffViewerOf(tenant.Context{TenantID: uuid.New()})
	out = render(tv, &acker, &resolver)
	if strings.Contains(out, acker.String()) || strings.Contains(out, resolver.String()) || !strings.Contains(out, `"acked_by":null`) || !strings.Contains(out, `"resolved_by":null`) {
		t.Errorf("tenant viewer must get null actors: %s", out)
	}
	// A zero DTO (built without the helper) fails closed.
	raw, _ := json.Marshal(alertListItemDTO{AckedBy: &staffRef{}})
	if !strings.Contains(string(raw), `"acked_by":null`) {
		t.Errorf("zero ref must marshal null: %s", raw)
	}
}

// Security F2: only the canonical spelling is accepted. Every alternative spelling of an otherwise
// valid cursor (case-folded keys, duplicate keys, whitespace, reordered keys, escaped characters,
// non-canonical base64) is refused.
func TestAlertCursor_OnlyTheCanonicalSpellingIsAccepted(t *testing.T) {
	id := uuid.NewString()
	ts := time.Date(2026, 5, 5, 5, 5, 5, 0, time.UTC).UnixMicro()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	canon := fmt.Sprintf(`{"v":1,"t":%d,"i":%q,"f":"open||"}`, ts, id)
	if _, err := decodeAlertCursor(enc(canon), "open||"); err != nil {
		t.Fatalf("canonical form refused: %v", err)
	}
	variants := map[string]string{
		"uppercase key":    fmt.Sprintf(`{"V":1,"t":%d,"i":%q,"f":"open||"}`, ts, id),
		"mixed-case key":   fmt.Sprintf(`{"v":1,"T":%d,"i":%q,"f":"open||"}`, ts, id),
		"duplicate key":    fmt.Sprintf(`{"v":1,"v":1,"t":%d,"i":%q,"f":"open||"}`, ts, id),
		"duplicate shadow": fmt.Sprintf(`{"v":1,"t":1,"t":%d,"i":%q,"f":"open||"}`, ts, id),
		"whitespace":       fmt.Sprintf(`{"v": 1,"t":%d,"i":%q,"f":"open||"}`, ts, id),
		"leading space":    " " + canon,
		"trailing newline": canon + "\n",
		"reordered keys":   fmt.Sprintf(`{"t":%d,"v":1,"i":%q,"f":"open||"}`, ts, id),
		"escaped filter":   fmt.Sprintf(`{"v":1,"t":%d,"i":%q,"f":"open\u007c\u007c"}`, ts, id),
		"escaped id":       fmt.Sprintf(`{"v":1,"t":%d,"i":"\u0030%s","f":"open||"}`, ts, id[1:]),
		"exponent number":  fmt.Sprintf(`{"v":1,"t":%de0,"i":%q,"f":"open||"}`, ts, id),
		"leading zero v":   fmt.Sprintf(`{"v":01,"t":%d,"i":%q,"f":"open||"}`, ts, id),
	}
	for name, v := range variants {
		if _, err := decodeAlertCursor(enc(v), "open||"); err == nil {
			t.Errorf("%s must be refused: %s", name, v)
		}
	}
	// Non-canonical base64: padding, and trailing bits that Go's lenient decoder would drop.
	good := enc(canon)
	if _, err := decodeAlertCursor(good+"=", "open||"); err == nil {
		t.Error("padded base64 must be refused")
	}
	if rem := len(canon) % 3; rem != 0 {
		alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		last := strings.IndexByte(alphabet, good[len(good)-1])
		altered := good[:len(good)-1] + string(alphabet[last^1])
		if _, err := decodeAlertCursor(altered, "open||"); err == nil {
			t.Error("base64 with non-zero trailing bits must be refused")
		}
	}
}
