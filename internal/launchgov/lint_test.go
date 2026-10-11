package launchgov

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

func TestLintFreeText_RefusesPIIShapes(t *testing.T) {
	bad := map[string]string{
		"email":            "contact jane.doe@example.com for details",
		"phone intl":       "call +44 20 7946 0958 today",
		"phone local":      "(555) 123-4567 ext",
		"iban":             "account GB82 WEST 1234 5698 7654 32",
		"iban compact":     "GB82WEST12345698765432",
		"pan":              "card 4111 1111 1111 1111 used",
		"long digit run":   "reference 123456789 was used",
		"digits with dash": "ref 12-34-56-78-9",
		"evm address":      "wallet 0x52908400098527886E0F7030069857D2E4169EE7",
		"bech32 address":   "send to bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq",
		"base58 address":   "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2",
		"tron address":     "TJRabPrwbZy45sbavfcjinPJC18kjpRTv8",
		"control char":     "bad\x07note",
	}
	for name, s := range bad {
		if err := LintFreeText("conditions_note", s, 2000); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %q must be refused, got %v", name, s, err)
		}
	}
	good := []string{
		"review again after the first quarter",
		"approved subject to the external counsel memo dated 2026-10-10",
		"phase 1 of 3; 8 digits only 12345678",
		"line one\nline two",
		"",
	}
	for _, s := range good {
		if err := LintFreeText("conditions_note", s, 2000); err != nil {
			t.Errorf("%q must be accepted, got %v", s, err)
		}
	}
}

// Security C3: the reviewer's exact evasion strings (and their relatives) are refused.
func TestLintFreeText_RefusesTheReviewersEvasions(t *testing.T) {
	bad := map[string]string{
		"pan with slashes":             "4111/1111/1111/1111",
		"pan with underscores":         "4111_1111_1111_1111",
		"pan with dots":                "4111.1111.1111.1111",
		"pan with mixed separators":    "4111 / 1111 _ 1111 . 1111",
		"full-width digits":            "４１１１１１１１１１１１１１１１",
		"full-width digits, long run":  "ref １２３４５６７８９０",
		"full-width at sign":           "jane＠example.com",
		"full-width everything":        "ｊａｎｅ＠ｅｘａｍｐｌｅ．ｃｏｍ",
		"at / dot spelled":             "x at example dot com",
		"(at) / (dot)":                 "x (at) example (dot) com",
		"[at] / [dot]":                 "x [at] example [dot] com",
		"zero-width inside the number": "4111​1111​1111​1111",
		"arabic-indic digits":          "٤١١١١١١١١١١١١١١١",
		"digits in brackets":           "(123)(456)(789)",
	}
	for name, s := range bad {
		if err := LintFreeText("conditions_note", s, 2000); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %q must be refused, got %v", name, s, err)
		}
	}
	// Not refused: ordinary prose that merely contains "at" or numbers below the run threshold.
	for _, ok := range []string{
		"meet at the office to review",
		"look at the second quarter figures",
		"clause 12 and 14, page 3",
		"phase 1/2/3 of the plan",
		"the 2026-10-10 memo",
		"version 1.2.3",
	} {
		if err := LintFreeText("conditions_note", ok, 2000); err != nil {
			t.Errorf("%q must be accepted, got %v", ok, err)
		}
	}
}

// Security C3: the registration field is a registry number, not free text.
func TestLintRegistration_RefusesPhonesNamesAndPunctuation(t *testing.T) {
	for _, bad := range []string{
		"+34 600 123 456",
		"12345678Z Jane Doe born 1980",
		"Jane Doe",
		"(555) 123-4567",
		"ceo at example dot com",
		"REG#4711",
		"REG_4711",
		"REG,4711",
		"-REG-4711",
		" REG-4711",
		"４１１１ １１１１ １１１１ １１１１",
		"4111/1111/1111/1111", // allowed characters, Luhn-valid card number
		"4111.1111.1111.1111",
	} {
		if err := LintRegistration(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q must be refused, got %v", bad, err)
		}
	}
	for _, ok := range []string{"", "REG-4711", "HRB 123456", "C 12345-2020", "CHE-123.456.789", "12345678901", "B/12345/2020"} {
		if err := LintRegistration(ok); err != nil {
			t.Errorf("registry number %q must be accepted: %v", ok, err)
		}
	}
}

func TestOtherDocumentedNeedsNote(t *testing.T) {
	if err := otherDocumentedNeedsNote(ReasonOtherDocumented, "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("other_documented with a blank note must be refused: %v", err)
	}
	if err := otherDocumentedNeedsNote(ReasonOtherDocumented, "see the counsel memo"); err != nil {
		t.Fatal(err)
	}
	if err := otherDocumentedNeedsNote("compliance_hold", ""); err != nil {
		t.Fatalf("every other reason needs no note: %v", err)
	}
}

func TestLintFreeText_SizeBounds(t *testing.T) {
	if err := LintFreeText("conditions_note", strings.Repeat("a", 2000), 2000); err != nil {
		t.Fatalf("2000 bytes is the bound: %v", err)
	}
	if err := LintFreeText("conditions_note", strings.Repeat("a", 2001), 2000); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("2001 bytes must be refused: %v", err)
	}
	if err := LintFreeText("x", "\xff\xfe", 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid UTF-8 must be refused: %v", err)
	}
	if err := trimmedNonEmpty("f", "   ", 10); err == nil {
		t.Fatal("blank must be refused")
	}
	if err := trimmedNonEmpty("f", strings.Repeat("a", 11), 10); err == nil {
		t.Fatal("over-long must be refused")
	}
}

func TestLintRegistration_AllowsRegistryNumbers_RefusesCardsAndEmail(t *testing.T) {
	for _, ok := range []string{"REG-4711", "12345678901", "HRB 123456", "C 12345-2020"} {
		if err := LintRegistration(ok); err != nil {
			t.Errorf("registry number %q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"4111 1111 1111 1111", "ceo@example.com", "GB82WEST12345698765432", strings.Repeat("x", 101)} {
		if err := LintRegistration(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q must be refused, got %v", bad, err)
		}
	}
}

func TestReasonCatalogue_IsClosedPerAction(t *testing.T) {
	for _, c := range []struct {
		action, code string
		ok           bool
	}{
		{ActionActivate, "initial_launch", true}, {ActionActivate, "compliance_hold", false},
		{ActionSuspend, "compliance_hold", true}, {ActionSuspend, "initial_launch", false},
		{ActionClose, "offboarding", true}, {ActionRatify, "legacy_ratification", true},
		{ActionReactivate, "issue_resolved", true}, {ActionReactivate, "", false}, {"nope", "x", false},
	} {
		if got := reasonAllowed(requestReasons, c.action, c.code); got != c.ok {
			t.Errorf("%s/%q = %v, want %v", c.action, c.code, got, c.ok)
		}
	}
	if !reasonAllowed(decisionReasons, DecisionApprove, "reviewed_ok") || reasonAllowed(decisionReasons, DecisionApprove, "policy_concern") ||
		!reasonAllowed(decisionReasons, DecisionReject, "policy_concern") {
		t.Fatal("decision reasons are closed per decision")
	}
}

func TestReadiness_HashIsDeterministic_IgnoresComputedAtAndOrder_BindsEveryResult(t *testing.T) {
	mk := func(res string) Readiness {
		return Readiness{Version: 1, Environment: "test", Checks: []Check{
			{Code: CheckAlerts, Result: ResultFail, DetailCode: "d"},
			{Code: CheckJurisdictionConfig, Result: res, DetailCode: "d"},
		}}
	}
	a := mk(ResultPass)
	b := mk(ResultPass)
	b.Checks[0], b.Checks[1] = b.Checks[1], b.Checks[0]
	if a.Hash() != b.Hash() {
		t.Fatal("check order must not change the hash")
	}
	b.ComputedAt = a.ComputedAt.AddDate(1, 0, 0)
	if a.Hash() != b.Hash() {
		t.Fatal("computed_at must not change the hash")
	}
	if a.Hash() == mk(ResultFail).Hash() || a.Hash() == mk(ResultNotEvaluable).Hash() {
		t.Fatal("every check result must change the hash")
	}
	c := mk(ResultPass)
	c.Environment = "production"
	if a.Hash() == c.Hash() {
		t.Fatal("the environment must change the hash")
	}
	d := mk(ResultPass)
	d.Checks[0].DetailCode = "other"
	if a.Hash() == d.Hash() {
		t.Fatal("the detail code must change the hash")
	}
	if got := a.NonPassing(); len(got) != 1 || got[0] != CheckAlerts {
		t.Fatalf("NonPassing: %v", got)
	}
	// not_evaluable is NOT pass.
	if got := mk(ResultNotEvaluable).NonPassing(); len(got) != 2 {
		t.Fatalf("not_evaluable must count as non-passing: %v", got)
	}
	if _, err := a.SnapshotJSON(); err != nil {
		t.Fatal(err)
	}
}

func TestAckRule_ExactlyTheNonPassingSet(t *testing.T) {
	r := Readiness{Checks: []Check{
		{Code: "R-A", Result: ResultPass}, {Code: "R-B", Result: ResultFail}, {Code: "R-C", Result: ResultNotEvaluable},
	}}
	if err := ackErr(r, []string{"R-B", "R-C"}); err != nil {
		t.Fatalf("exact set must be accepted: %v", err)
	}
	var pe *PreconditionError
	if err := ackErr(r, []string{"R-B"}); !errors.Is(err, ErrUnacknowledged) || !errors.As(err, &pe) || pe.Codes[0] != "R-C" {
		t.Fatalf("a missing acknowledgement must be refused naming the check: %v", err)
	}
	if err := ackErr(r, []string{"R-B", "R-C", "R-A"}); !errors.Is(err, ErrUnacknowledged) {
		t.Fatalf("a blanket acknowledgement of a passing check must be refused: %v", err)
	}
	if err := ackErr(r, []string{"R-B", "R-C", "R-NOPE"}); !errors.Is(err, ErrUnacknowledged) {
		t.Fatalf("an unknown acknowledged code must be refused: %v", err)
	}
	if err := ackErr(Readiness{Checks: []Check{{Code: "R-A", Result: ResultPass}}}, nil); err != nil {
		t.Fatalf("all pass: no acknowledgement needed: %v", err)
	}
}

func TestMoveFor_MatchesTheStateMachine(t *testing.T) {
	type c struct {
		action, from, to string
		ok               bool
	}
	for _, x := range []c{
		{ActionActivate, StatusPendingLaunch, StatusActive, true}, {ActionActivate, StatusActive, "", false}, {ActionActivate, StatusSuspended, "", false},
		{ActionSuspend, StatusActive, StatusSuspended, true}, {ActionSuspend, StatusPendingLaunch, "", false}, {ActionSuspend, StatusSuspended, "", false},
		{ActionReactivate, StatusSuspended, StatusActive, true}, {ActionReactivate, StatusPendingLaunch, "", false},
		{ActionClose, StatusPendingLaunch, StatusClosed, true}, {ActionClose, StatusActive, StatusClosed, true}, {ActionClose, StatusSuspended, StatusClosed, true}, {ActionClose, StatusClosed, "", false},
		{ActionRatify, StatusActive, StatusActive, true}, {ActionRatify, StatusSuspended, StatusSuspended, true}, {ActionRatify, StatusPendingLaunch, "", false},
		{"nope", StatusActive, "", false},
	} {
		to, ok := moveFor(x.action, x.from)
		if ok != x.ok || (ok && to != x.to) {
			t.Errorf("%s from %s = (%q,%v), want (%q,%v)", x.action, x.from, to, ok, x.to, x.ok)
		}
	}
}

func TestAuthorize_ScopeAndPermissionRules(t *testing.T) {
	sub := Subject{TenantID: uuid.MustParse("11111111-1111-4111-8111-111111111111")}
	brand := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	bsub := Subject{TenantID: sub.TenantID, BrandID: &brand}
	staff := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	other := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	req, appr := auth.PermLaunchAuthorisationRequest, auth.PermLaunchAuthorisationApprove

	pa := Actor{StaffID: staff, Scope: ScopePlatform, Role: "platform_admin"}
	ta := Actor{StaffID: staff, Scope: ScopeTenant, TenantID: sub.TenantID, Role: "tenant_admin"}
	cases := []struct {
		name  string
		a     Actor
		perm  auth.Permission
		sub   Subject
		brand bool
		want  error
	}{
		{"platform request tenant", pa, req, sub, true, nil},
		{"platform approve", pa, appr, sub, false, nil},
		{"tenant request brand", ta, req, bsub, true, nil},
		{"tenant request tenant subject", ta, req, sub, true, ErrForbidden},
		{"tenant approve", ta, appr, bsub, false, ErrForbidden},
		{"tenant foreign tenant", ta, req, Subject{TenantID: other, BrandID: &brand}, true, ErrNotFound},
		{"nil actor", Actor{Scope: ScopePlatform, Role: "platform_admin"}, req, sub, true, ErrForbidden},
		{"acting scope", Actor{StaffID: staff, Scope: "platform_acting", Role: "platform_admin"}, req, sub, true, ErrActingSession},
		{"unknown scope", Actor{StaffID: staff, Scope: "service", Role: "platform_admin"}, req, sub, true, ErrForbidden},
		{"platform with tenant", Actor{StaffID: staff, Scope: ScopePlatform, TenantID: other, Role: "platform_admin"}, req, sub, true, ErrForbidden},
		{"tenant scope without tenant", Actor{StaffID: staff, Scope: ScopeTenant, Role: "tenant_admin"}, req, bsub, true, ErrForbidden},
		{"unknown role", Actor{StaffID: staff, Scope: ScopePlatform, Role: "nobody"}, req, sub, true, ErrForbidden},
		{"finance role", Actor{StaffID: staff, Scope: ScopeTenant, TenantID: sub.TenantID, Role: "finance"}, req, bsub, true, ErrForbidden},
		{"no tenant id", pa, req, Subject{}, true, ErrInvalidInput},
	}
	for _, c := range cases {
		err := authorize(c.a, c.perm, c.sub, c.brand)
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
}

// The LA014 detail is parsed into the matching prior request; an unreadable detail still yields a
// real retry time (never the zero time, year 0001).
func TestPriorReject_FromDetail_AndFallbackIsNeverTheZeroTime(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	svc := &Service{cfg: Config{Clock: func() time.Time { return now }}}
	id := uuid.New()
	e := svc.priorReject(&pgconn.PgError{Code: "LA014", Detail: "prior_request_id=" + id.String() + " decided_at=2026-10-10T08:30:00.123456Z"}).(*PriorRejectError)
	if e.RequestID != id || !e.DecidedAt.Equal(time.Date(2026, 10, 10, 8, 30, 0, 123456000, time.UTC)) || !e.RetryAfter.Equal(e.DecidedAt.Add(RequestTTL)) {
		t.Fatalf("parsed: %+v", e)
	}
	for _, detail := range []string{"", "garbage", "prior_request_id=not-a-uuid decided_at=nope"} {
		e := svc.priorReject(&pgconn.PgError{Code: "LA014", Detail: detail}).(*PriorRejectError)
		if e.RetryAfter.Year() < 2026 || e.DecidedAt.IsZero() || !e.RetryAfter.Equal(now.Add(RequestTTL)) {
			t.Fatalf("fallback for %q must be a real time (now + TTL): %+v", detail, e)
		}
	}
}

// R-ALERTS is derived from the alert channel vocabulary: it passes exactly when some known channel
// kind notifies a person. Today none does (ALERT-DELIVERY-1 OPEN), so it fails; the expectation is
// computed from the vocabulary, not restated.
func TestAlertsCheck_IsDerivedFromTheChannelVocabulary(t *testing.T) {
	human := false
	for _, k := range alerting.AllChannelKinds() {
		if alerting.KnownHumanChannelKind(k) {
			human = true
		}
	}
	got := alertsCheck()
	want := ResultFail
	if human {
		want = ResultPass
	}
	if got.Code != CheckAlerts || got.Result != want {
		t.Fatalf("R-ALERTS = %+v, want result %s", got, want)
	}
	if len(alerting.AllChannelKinds()) < 2 {
		t.Fatal("the vocabulary must list the known channel kinds")
	}
	if !human && got.DetailCode != "no_human_notification_channel" {
		t.Fatalf("a failing R-ALERTS names its reason: %+v", got)
	}
}
