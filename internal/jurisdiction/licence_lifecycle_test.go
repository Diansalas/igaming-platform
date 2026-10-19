package jurisdiction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

func d(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestClassifyStatusChange(t *testing.T) {
	cases := []struct {
		from, to string
		want     ChangeKind
		bad      bool
	}{
		{"active", "suspended", ChangeNarrowing, false},
		{"active", "expired", ChangeNarrowing, false},
		{"suspended", "expired", ChangeNarrowing, false},
		{"suspended", "active", ChangeWidening, false},
		{"expired", "active", ChangeWidening, false},
		{"expired", "suspended", ChangeWidening, false},
		{"active", "active", "", true},
		{"active", "revoked", "", true}, // not in the vocabulary
		{"active", "", "", true},
		{"weird", "suspended", ChangeWidening, false}, // unknown stored status fails toward the stricter control
	}
	for _, c := range cases {
		got, err := ClassifyStatusChange(c.from, c.to)
		if c.bad {
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("%s->%s: want ErrInvalidInput, got %v", c.from, c.to, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s->%s: got %q %v want %q", c.from, c.to, got, err, c.want)
		}
	}
}

func TestClassifyValidityDatesChange(t *testing.T) {
	cases := []struct {
		name           string
		oi, oe, ni, ne *time.Time
		want           ChangeKind
		bad            bool
	}{
		{"extend expiry", nil, d("2027-01-01"), nil, d("2028-01-01"), ChangeWidening, false},
		{"shorten expiry", nil, d("2028-01-01"), nil, d("2027-01-01"), ChangeNarrowing, false},
		{"set expiry from none", nil, nil, nil, d("2027-01-01"), ChangeNarrowing, false},
		{"clear expiry", nil, d("2027-01-01"), nil, nil, ChangeWidening, false},
		{"issue date later", d("2026-01-01"), nil, d("2026-06-01"), nil, ChangeNarrowing, false},
		{"issue date earlier", d("2026-06-01"), nil, d("2026-01-01"), nil, ChangeWidening, false},
		{"set issue date from none", nil, nil, d("2026-01-01"), nil, ChangeNarrowing, false},
		{"mixed is widening", d("2026-06-01"), d("2027-01-01"), d("2026-07-01"), d("2028-01-01"), ChangeWidening, false},
		{"unchanged", d("2026-06-01"), d("2027-01-01"), d("2026-06-01"), d("2027-01-01"), "", true},
	}
	for _, c := range cases {
		got, err := ClassifyValidityDatesChange(c.oi, c.oe, c.ni, c.ne)
		if c.bad {
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("%s: want ErrInvalidInput, got %v", c.name, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q %v want %q", c.name, got, err, c.want)
		}
	}
}

func TestClassifyReviewDueChange(t *testing.T) {
	if k, _ := ClassifyReviewDueChange(d("2026-12-01"), d("2027-03-01")); k != ChangeWidening {
		t.Errorf("postpone: %q", k)
	}
	if k, _ := ClassifyReviewDueChange(d("2026-12-01"), d("2026-11-01")); k != ChangeNarrowing {
		t.Errorf("bring forward: %q", k)
	}
	if k, _ := ClassifyReviewDueChange(nil, d("2026-11-01")); k != ChangeNarrowing {
		t.Errorf("set: %q", k)
	}
	if k, _ := ClassifyReviewDueChange(d("2026-11-01"), nil); k != ChangeWidening {
		t.Errorf("clear: %q", k)
	}
	if _, err := ClassifyReviewDueChange(nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unchanged: %v", err)
	}
}

func TestValidateEvidenceReference(t *testing.T) {
	for _, ok := range []string{"REG-LETTER-2026-10-01", "ref 42"} {
		if err := validateEvidenceReference(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " x", "x ", "a@b.example", "line\nbreak", strings.Repeat("x", 201)} {
		if err := validateEvidenceReference(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q: want ErrInvalidInput, got %v", bad, err)
		}
	}
}

func TestClassifyLicenceForSweep(t *testing.T) {
	now := time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		status      string
		exp, review *time.Time
		wantKind    alerting.Kind
		wantTrigger string
	}{
		{"status expired", "expired", nil, nil, alerting.KindLicenceExpired, TriggerStatusExpired},
		{"expires today is expired (same boundary as EvaluateLicenceValidity)", "active", d("2026-10-19"), nil, alerting.KindLicenceExpired, TriggerExpiresAtReached},
		{"expired yesterday", "active", d("2026-10-18"), nil, alerting.KindLicenceExpired, TriggerExpiresAtReached},
		{"expires tomorrow is expiring", "active", d("2026-10-20"), nil, alerting.KindLicenceExpiring, TriggerExpiresAtSoon},
		{"expires on the horizon (today+30d)", "active", d("2026-11-18"), nil, alerting.KindLicenceExpiring, TriggerExpiresAtSoon},
		{"expires after the horizon", "active", d("2026-11-19"), nil, "", ""},
		{"review due inside the window", "active", d("2027-06-01"), d("2026-11-01"), alerting.KindLicenceExpiring, TriggerReviewDue},
		{"review overdue", "active", nil, d("2026-09-01"), alerting.KindLicenceExpiring, TriggerReviewDue},
		{"review after window", "active", nil, d("2026-12-31"), "", ""},
		{"nothing recorded", "active", nil, nil, "", ""},
		{"suspended with passed expiry is still expired", "suspended", d("2026-01-01"), nil, alerting.KindLicenceExpired, TriggerExpiresAtReached},
	}
	for _, c := range cases {
		k, tr := ClassifyLicenceForSweep(c.status, c.exp, c.review, now, LicenceExpiryWarningWindowDays)
		if k != c.wantKind || tr != c.wantTrigger {
			t.Errorf("%s: got %q/%q want %q/%q", c.name, k, tr, c.wantKind, c.wantTrigger)
		}
	}
}

func TestFindingAttributes_BoundedAndWithinTheAlertPayloadLimit(t *testing.T) {
	f := LicenceFinding{Kind: alerting.KindLicenceExpired, Trigger: TriggerStatusExpired, ExpiresAt: d("2026-01-01"), ReviewDueAt: d("2026-02-01")}
	for i := 0; i < 100; i++ {
		f.TenantIDs = append(f.TenantIDs, uuid.New())
		f.BrandIDs = append(f.BrandIDs, uuid.New())
	}
	attrs := findingAttributes(f)
	attrs["licence_id"] = uuid.New().String()
	b, err := json.Marshal(attrs)
	if err != nil || len(b) > 2048 {
		t.Fatalf("payload %d bytes (limit 2048), err %v", len(b), err)
	}
	if attrs["tenant_count"] != 100 || attrs["brand_count"] != 100 || attrs["truncated"] != true {
		t.Fatalf("counts must stay exact and truncation visible: %v", attrs)
	}
	if n := strings.Count(attrs["tenant_ids"].(string), ",") + 1; n != maxAlertIDs {
		t.Fatalf("tenant_ids has %d ids, want %d", n, maxAlertIDs)
	}
}
