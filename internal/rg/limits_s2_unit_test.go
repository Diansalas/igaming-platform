package rg

// ADR 0115 S2 unit tests (no database): the validation logic of the limit write APIs - the pure parts
// the mutation pass targets (value / key syntax, ceiling and period decisions, configuration shape
// rules, the SQLSTATE -> refusal mapping, request fingerprints).

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func sp(s string) *string         { return &s }
func pp(p PeriodKind) *PeriodKind { return &p }
func bp(b bool) *bool             { return &b }
func ip(i int64) *int64           { return &i }

func goodAmount() LimitInput {
	return LimitInput{Kind: ControlDeposit, AssetCode: sp("EUR"), Period: pp(PeriodRolling24h), Value: sp("1000"), IdempotencyKey: "k-1"}
}

func wantInvalid(t *testing.T, err error, why string) {
	t.Helper()
	r, ok := IsRefusal(err)
	if !ok || r.Code != RefusedInvalid {
		t.Fatalf("%s: want an invalid refusal, got %v", why, err)
	}
}

func TestLimitInputValidate_Accepts(t *testing.T) {
	if err := goodAmount().validate(); err != nil {
		t.Fatal(err)
	}
	removal := goodAmount()
	removal.Value = nil
	if err := removal.validate(); err != nil {
		t.Fatalf("a removal (nil value) is valid input: %v", err)
	}
	sess := LimitInput{Kind: ControlSessionDuration, Value: sp("3600"), IdempotencyKey: "k"}
	if err := sess.validate(); err != nil {
		t.Fatal(err)
	}
	big := goodAmount()
	big.Value = sp(strings.Repeat("9", 38))
	if err := big.validate(); err != nil {
		t.Fatalf("38 digits fit NUMERIC(38,0): %v", err)
	}
}

func TestLimitInputValidate_Rejects(t *testing.T) {
	mut := func(f func(*LimitInput)) LimitInput { in := goodAmount(); f(&in); return in }
	cases := map[string]LimitInput{
		"kind not a player kind (reality check)": mut(func(i *LimitInput) { i.Kind = ControlRealityCheckInterval }),
		"kind not a player kind (time_out)":      mut(func(i *LimitInput) { i.Kind = ControlTimeOut }),
		"kind empty":                             mut(func(i *LimitInput) { i.Kind = "" }),
		"amount without asset":                   mut(func(i *LimitInput) { i.AssetCode = nil }),
		"asset with a space":                     mut(func(i *LimitInput) { i.AssetCode = sp("E UR") }),
		"asset empty":                            mut(func(i *LimitInput) { i.AssetCode = sp("") }),
		"asset 33 chars":                         mut(func(i *LimitInput) { i.AssetCode = sp(strings.Repeat("A", 33)) }),
		"amount without period":                  mut(func(i *LimitInput) { i.Period = nil }),
		"unknown period":                         mut(func(i *LimitInput) { i.Period = pp("hourly") }),
		"session with asset":                     {Kind: ControlSessionDuration, AssetCode: sp("EUR"), Value: sp("1"), IdempotencyKey: "k"},
		"session with period":                    {Kind: ControlSessionDuration, Period: pp(PeriodRolling24h), Value: sp("1"), IdempotencyKey: "k"},
		"zero":                                   mut(func(i *LimitInput) { i.Value = sp("0") }),
		"negative":                               mut(func(i *LimitInput) { i.Value = sp("-5") }),
		"leading zero":                           mut(func(i *LimitInput) { i.Value = sp("01") }),
		"decimal":                                mut(func(i *LimitInput) { i.Value = sp("1.5") }),
		"exponent":                               mut(func(i *LimitInput) { i.Value = sp("1e3") }),
		"39 digits":                              mut(func(i *LimitInput) { i.Value = sp(strings.Repeat("9", 39)) }),
		"empty value":                            mut(func(i *LimitInput) { i.Value = sp("") }),
		"value with newline":                     mut(func(i *LimitInput) { i.Value = sp("1\n") }),
		"key empty":                              mut(func(i *LimitInput) { i.IdempotencyKey = "" }),
		"key with space":                         mut(func(i *LimitInput) { i.IdempotencyKey = "a b" }),
		"key 121 chars":                          mut(func(i *LimitInput) { i.IdempotencyKey = strings.Repeat("k", 121) }),
		"key non-ascii":                          mut(func(i *LimitInput) { i.IdempotencyKey = "ké" }),
	}
	for name, in := range cases {
		wantInvalid(t, in.validate(), name)
	}
	// the boundary values themselves are accepted
	max := goodAmount()
	max.AssetCode = sp(strings.Repeat("A", 32))
	if err := max.validate(); err != nil {
		t.Fatalf("a 32-char asset code is the maximum accepted: %v", err)
	}
	ok := goodAmount()
	ok.IdempotencyKey = strings.Repeat("k", 120)
	if err := ok.validate(); err != nil {
		t.Fatalf("a 120-char key is the maximum accepted: %v", err)
	}
}

func TestLimitInputHash_BindsEveryFieldAndNotTheKey(t *testing.T) {
	base := goodAmount().hash("player")
	if base != goodAmount().hash("player") {
		t.Fatal("the fingerprint must be deterministic")
	}
	k := goodAmount()
	k.IdempotencyKey = "another-key"
	if k.hash("player") != base {
		t.Fatal("the idempotency key itself is not part of the fingerprint")
	}
	diff := map[string]LimitInput{}
	m := func(name string, f func(*LimitInput)) { in := goodAmount(); f(&in); diff[name] = in }
	m("kind", func(i *LimitInput) { i.Kind = ControlLoss })
	m("asset", func(i *LimitInput) { i.AssetCode = sp("USD") })
	m("period", func(i *LimitInput) { i.Period = pp(PeriodRolling7d) })
	m("value", func(i *LimitInput) { i.Value = sp("1001") })
	m("removal", func(i *LimitInput) { i.Value = nil })
	seen := map[string]string{base: "base"}
	for name, in := range diff {
		h := in.hash("player")
		if prev, dup := seen[h]; dup {
			t.Fatalf("%s collides with %s", name, prev)
		}
		seen[h] = name
	}
	if goodAmount().hash("player") == goodAmount().hash("staff", "r", "e") {
		t.Fatal("extra parts (actor class, reason, evidence) must change the fingerprint")
	}
	// length-prefixed parts: ("ab","c") != ("a","bc")
	if fingerprint("ab", "c") == fingerprint("a", "bc") {
		t.Fatal("fingerprint parts must be length-prefixed")
	}
	if fingerprint("a|b", "c") == fingerprint("a", "b|c") {
		t.Fatal("a separator inside a part must not let two different requests collide")
	}
}

func TestExceedsCeiling(t *testing.T) {
	cfg := func(c string) ComposedConfig { return ComposedConfig{Found: true, Ceiling: &c} }
	cases := []struct {
		name  string
		cfg   ComposedConfig
		value *string
		want  bool
	}{
		{"below", cfg("1000"), sp("999"), false},
		{"equal is allowed", cfg("1000"), sp("1000"), false},
		{"one above", cfg("1000"), sp("1001"), true},
		{"numeric not lexicographic", cfg("9"), sp("10"), true},
		{"numeric not lexicographic (reverse)", cfg("10"), sp("9"), false},
		{"38 digit ceiling", cfg(strings.Repeat("9", 38)), sp(strings.Repeat("9", 38)), false},
		{"removal never exceeds", cfg("1"), nil, false},
		{"no configuration", ComposedConfig{}, sp("999999"), false},
		{"not found never exceeds even with a stale ceiling", ComposedConfig{Found: false, Ceiling: sp("1")}, sp("999999"), false},
		{"attested none ceiling", ComposedConfig{Found: true, CeilingNone: true}, sp("999999"), false},
	}
	for _, c := range cases {
		got, err := c.cfg.exceedsCeiling(c.value)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v want %v", c.name, got, err, c.want)
		}
	}
	bad := "not-a-number"
	if _, err := cfg(bad).exceedsCeiling(sp("1")); err == nil {
		t.Error("an unparsable ceiling must be an error (fail closed)")
	}
}

func TestPeriodRefused(t *testing.T) {
	cfg := ComposedConfig{Found: true, AllowedPeriods: []string{"rolling_24h", "calendar_day"}}
	in := goodAmount()
	if cfg.periodRefused(in) {
		t.Error("an allowed period must pass")
	}
	in.Period = pp(PeriodRolling7d)
	if !cfg.periodRefused(in) {
		t.Error("a period outside allowed_periods must be refused")
	}
	if (ComposedConfig{}).periodRefused(in) {
		t.Error("with no configuration in force there is nothing to refuse against")
	}
	sess := LimitInput{Kind: ControlSessionDuration, Value: sp("1"), IdempotencyKey: "k"}
	if cfg.periodRefused(sess) {
		t.Error("a session limit has no period")
	}
}

func TestDurationAllowed(t *testing.T) {
	cases := []struct {
		name  string
		cfg   ComposedConfig
		hours int
		want  bool
	}{
		{"no configuration: any positive duration", ComposedConfig{}, 7, true},
		{"any", ComposedConfig{Found: true, AllowedDurationsAny: true}, 7, true},
		{"any wins over a stale list", ComposedConfig{Found: true, AllowedDurationsAny: true, AllowedDurationsSecs: []int64{86400}}, 7, true},
		{"configured and listed", ComposedConfig{Found: true, AllowedDurationsSecs: []int64{86400, 259200}}, 24, true},
		{"configured and listed (second)", ComposedConfig{Found: true, AllowedDurationsSecs: []int64{86400, 259200}}, 72, true},
		{"configured and not listed", ComposedConfig{Found: true, AllowedDurationsSecs: []int64{86400}}, 23, false},
		{"hours are converted to seconds", ComposedConfig{Found: true, AllowedDurationsSecs: []int64{3600}}, 1, true},
	}
	for _, c := range cases {
		if got := c.cfg.durationAllowed(c.hours); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// ---- configuration shape --------------------------------------------------------------------

func baseContent(kind ControlKind) ConfigContent {
	c := ConfigContent{ControlKind: kind, Enabled: bp(true), Mandatory: bp(false), ReasonCode: "r", AttestationRef: "att-1"}
	limit := func() {
		c.Ceiling = &Attested[string]{None: true}
		c.IncreaseCooldownSeconds = ip(3600)
		c.IncreaseRequiresReconfirmation = bp(true)
		c.Scope = sp("account")
	}
	amount := func() {
		c.AllowedPeriods = []string{"rolling_24h", "calendar_day"}
		c.CalendarTZ = sp("UTC")
		w := 1
		c.WeekStart = &w
	}
	switch kind {
	case ControlDeposit:
		limit()
		amount()
		c.PendingIntentTTLSeconds = &Attested[int64]{None: true}
	case ControlLoss:
		limit()
		amount()
		c.CountBonusFunded = bp(false)
		c.LossComputation = sp("net_of_wins")
		c.LossMayGoNegative = &Attested[bool]{None: true}
	case ControlWager:
		limit()
		amount()
		c.CountBonusFunded = bp(true)
	case ControlSessionDuration:
		limit()
		c.PostSessionBreakSeconds = &Attested[int64]{None: true}
		c.ConcurrentChainAggregation = &Attested[bool]{None: true}
	case ControlTimeOut:
		c.AllowedDurationsAny = bp(true)
		c.Scope = sp("platform_person")
	case ControlRealityCheckInterval:
		c.AllowedIntervalsSeconds = []int64{900, 1800}
		c.Scope = sp("account")
	}
	return c
}

func TestConfigContentValidate_EveryKindAcceptsItsBaseline(t *testing.T) {
	for _, k := range ConfigControlKinds {
		c := baseContent(k)
		if k.IsAmountKind() {
			c.AssetCode = sp("EUR")
			c.Ceiling = &Attested[string]{Value: sp("5000")}
		}
		if err := c.Validate(); err != nil {
			t.Errorf("%s baseline: %v", k, err)
		}
	}
}

func TestConfigContentValidate_Rejects(t *testing.T) {
	type mutation struct {
		name string
		kind ControlKind
		f    func(*ConfigContent)
	}
	ms := []mutation{
		{"unknown kind", ControlDeposit, func(c *ConfigContent) { c.ControlKind = "bogus" }},
		{"missing enabled", ControlWager, func(c *ConfigContent) { c.Enabled = nil }},
		{"missing mandatory", ControlWager, func(c *ConfigContent) { c.Mandatory = nil }},
		{"missing ceiling", ControlDeposit, func(c *ConfigContent) { c.Ceiling = nil }},
		{"missing cooldown", ControlDeposit, func(c *ConfigContent) { c.IncreaseCooldownSeconds = nil }},
		{"zero cooldown", ControlDeposit, func(c *ConfigContent) { c.IncreaseCooldownSeconds = ip(0) }},
		{"negative cooldown", ControlDeposit, func(c *ConfigContent) { c.IncreaseCooldownSeconds = ip(-1) }},
		{"cooldown above the sanity bound", ControlDeposit, func(c *ConfigContent) { c.IncreaseCooldownSeconds = ip(maxIntervalSeconds + 1) }},
		{"missing reconfirmation", ControlDeposit, func(c *ConfigContent) { c.IncreaseRequiresReconfirmation = nil }},
		{"missing scope", ControlDeposit, func(c *ConfigContent) { c.Scope = nil }},
		{"platform_person scope on a limit kind", ControlDeposit, func(c *ConfigContent) { c.Scope = sp("platform_person") }},
		{"missing periods", ControlDeposit, func(c *ConfigContent) { c.AllowedPeriods = nil }},
		{"duplicate period", ControlDeposit, func(c *ConfigContent) { c.AllowedPeriods = []string{"rolling_24h", "rolling_24h"} }},
		{"unknown period", ControlDeposit, func(c *ConfigContent) { c.AllowedPeriods = []string{"hourly"} }},
		{"missing tz", ControlDeposit, func(c *ConfigContent) { c.CalendarTZ = nil }},
		{"bad tz", ControlDeposit, func(c *ConfigContent) { c.CalendarTZ = sp("UTC; DROP") }},
		{"week start 0", ControlDeposit, func(c *ConfigContent) { w := 0; c.WeekStart = &w }},
		{"week start 8", ControlDeposit, func(c *ConfigContent) { w := 8; c.WeekStart = &w }},
		{"missing week start", ControlDeposit, func(c *ConfigContent) { c.WeekStart = nil }},
		{"all-assets row with a numeric ceiling", ControlDeposit, func(c *ConfigContent) { c.AssetCode = nil; c.Ceiling = &Attested[string]{Value: sp("5")} }},
		{"ceiling value zero", ControlDeposit, func(c *ConfigContent) { c.AssetCode = sp("EUR"); c.Ceiling = &Attested[string]{Value: sp("0")} }},
		{"ceiling value malformed", ControlDeposit, func(c *ConfigContent) { c.AssetCode = sp("EUR"); c.Ceiling = &Attested[string]{Value: sp("1.5")} }},
		{"ceiling value nil", ControlDeposit, func(c *ConfigContent) { c.AssetCode = sp("EUR"); c.Ceiling = &Attested[string]{} }},
		{"missing bonus flag", ControlWager, func(c *ConfigContent) { c.CountBonusFunded = nil }},
		{"bonus flag on a deposit", ControlDeposit, func(c *ConfigContent) { c.CountBonusFunded = bp(true) }},
		{"loss computation other than net_of_wins", ControlLoss, func(c *ConfigContent) { c.LossComputation = sp("gross") }},
		{"missing loss computation", ControlLoss, func(c *ConfigContent) { c.LossComputation = nil }},
		{"missing loss_may_go_negative", ControlLoss, func(c *ConfigContent) { c.LossMayGoNegative = nil }},
		{"loss field on a wager", ControlWager, func(c *ConfigContent) { c.LossComputation = sp("net_of_wins") }},
		{"missing post_session_break", ControlSessionDuration, func(c *ConfigContent) { c.PostSessionBreakSeconds = nil }},
		{"post_session_break zero", ControlSessionDuration, func(c *ConfigContent) { c.PostSessionBreakSeconds = &Attested[int64]{Value: ip(0)} }},
		{"missing concurrent_chain_aggregation", ControlSessionDuration, func(c *ConfigContent) { c.ConcurrentChainAggregation = nil }},
		{"session limit with a period list", ControlSessionDuration, func(c *ConfigContent) { c.AllowedPeriods = []string{"rolling_24h"} }},
		{"missing pending_intent_ttl", ControlDeposit, func(c *ConfigContent) { c.PendingIntentTTLSeconds = nil }},
		{"pending_intent_ttl with a value is not effective (F10)", ControlDeposit, func(c *ConfigContent) { c.PendingIntentTTLSeconds = &Attested[int64]{Value: ip(7200)} }},
		{"pending_intent_ttl negative", ControlDeposit, func(c *ConfigContent) { c.PendingIntentTTLSeconds = &Attested[int64]{Value: ip(-3)} }},
		{"time-out without any/durations", ControlTimeOut, func(c *ConfigContent) { c.AllowedDurationsAny = nil }},
		{"time-out any=false without durations", ControlTimeOut, func(c *ConfigContent) { c.AllowedDurationsAny = bp(false) }},
		{"time-out any=true with durations", ControlTimeOut, func(c *ConfigContent) { c.AllowedDurationsSeconds = []int64{60} }},
		{"time-out duplicate durations", ControlTimeOut, func(c *ConfigContent) { c.AllowedDurationsAny = bp(false); c.AllowedDurationsSeconds = []int64{60, 60} }},
		{"time-out zero duration", ControlTimeOut, func(c *ConfigContent) { c.AllowedDurationsAny = bp(false); c.AllowedDurationsSeconds = []int64{0} }},
		{"time-out unknown scope", ControlTimeOut, func(c *ConfigContent) { c.Scope = sp("bogus") }},
		{"limit kind unknown scope", ControlDeposit, func(c *ConfigContent) { c.Scope = sp("bogus") }},
		{"time-out with a ceiling", ControlTimeOut, func(c *ConfigContent) { c.Ceiling = &Attested[string]{None: true} }},
		{"reality check without intervals", ControlRealityCheckInterval, func(c *ConfigContent) { c.AllowedIntervalsSeconds = nil }},
		{"reality check duplicate intervals", ControlRealityCheckInterval, func(c *ConfigContent) { c.AllowedIntervalsSeconds = []int64{60, 60} }},
		{"reality check scope other than account", ControlRealityCheckInterval, func(c *ConfigContent) { c.Scope = sp("tenant_person") }},
		{"empty reason", ControlWager, func(c *ConfigContent) { c.ReasonCode = "" }},
		{"reason 65 chars", ControlWager, func(c *ConfigContent) { c.ReasonCode = strings.Repeat("r", 65) }},
		{"empty attestation", ControlWager, func(c *ConfigContent) { c.AttestationRef = "" }},
		{"attestation non-ASCII", ControlWager, func(c *ConfigContent) { c.AttestationRef = "signé" }},
		{"attestation control char", ControlWager, func(c *ConfigContent) { c.AttestationRef = "a\x00b" }},
		{"attestation 201 chars", ControlWager, func(c *ConfigContent) { c.AttestationRef = strings.Repeat("a", 201) }},
		{"asset on a session limit", ControlSessionDuration, func(c *ConfigContent) { c.AssetCode = sp("EUR") }},
		{"malformed asset", ControlWager, func(c *ConfigContent) { c.AssetCode = sp("E UR") }},
	}
	for _, m := range ms {
		c := baseContent(m.kind)
		if m.kind.IsAmountKind() && m.name != "all-assets row with a numeric ceiling" && !strings.Contains(m.name, "ceiling value") && !strings.Contains(m.name, "asset") {
			c.AssetCode = sp("EUR")
		}
		m.f(&c)
		wantInvalid(t, c.Validate(), m.name)
	}
	// attestation boundaries
	c := baseContent(ControlWager)
	c.AssetCode = sp("EUR")
	c.AttestationRef = strings.Repeat("a", 200)
	c.ReasonCode = strings.Repeat("r", 64)
	if err := c.Validate(); err != nil {
		t.Fatalf("200/64-char boundary values are accepted: %v", err)
	}
	w := 7
	c.WeekStart = &w
	if err := c.Validate(); err != nil {
		t.Fatalf("week_start 7 is accepted: %v", err)
	}
	w = 1
	c.IncreaseCooldownSeconds = ip(1)
	if err := c.Validate(); err != nil {
		t.Fatalf("a 1-second cooldown is accepted: %v", err)
	}
	c.IncreaseCooldownSeconds = ip(maxIntervalSeconds)
	if err := c.Validate(); err != nil {
		t.Fatalf("the sanity bound itself is accepted: %v", err)
	}
}

func TestConfigContentRow_ColumnsAndEncoding(t *testing.T) {
	c := baseContent(ControlLoss)
	c.AssetCode = sp("EUR")
	c.Ceiling = &Attested[string]{Value: sp("5000")}
	c.LossMayGoNegative = &Attested[bool]{Value: bp(true)}
	row := c.Row("MT", nil, nil)
	cols := make([]string, 0, len(row))
	for k := range row {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	want := append([]string(nil), configContentColumns...)
	sort.Strings(want)
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("Row must carry exactly the content columns:\n got %v\nwant %v", cols, want)
	}
	if row["ceiling"] != "5000" || row["ceiling_none"] != false {
		t.Errorf("a numeric ceiling: ceiling=%v none=%v", row["ceiling"], row["ceiling_none"])
	}
	if row["increase_cooldown"] != "3600 seconds" {
		t.Errorf("cooldown encoding: %v", row["increase_cooldown"])
	}
	if row["loss_may_go_negative"] != true || row["loss_may_go_negative_none"] != false {
		t.Errorf("loss_may_go_negative encoding: %v %v", row["loss_may_go_negative"], row["loss_may_go_negative_none"])
	}
	if row["tenant_id"] != nil || row["brand_id"] != nil || row["jurisdiction_code"] != "MT" {
		t.Errorf("floor row scope: %v %v %v", row["tenant_id"], row["brand_id"], row["jurisdiction_code"])
	}
	none := baseContent(ControlDeposit)
	none.AssetCode = sp("EUR")
	r2 := none.Row("MT", nil, nil)
	if r2["ceiling"] != nil || r2["ceiling_none"] != true {
		t.Errorf("an attested-none ceiling: ceiling=%v none=%v", r2["ceiling"], r2["ceiling_none"])
	}
	if r2["pending_intent_ttl"] != nil || r2["pending_intent_ttl_none"] != true {
		t.Errorf("an attested-none ttl: %v %v", r2["pending_intent_ttl"], r2["pending_intent_ttl_none"])
	}
	// the row must round-trip through JSON (it is sent to jsonb_populate_record)
	if _, err := json.Marshal(row); err != nil {
		t.Fatal(err)
	}
}

func TestAttestedJSON(t *testing.T) {
	var a Attested[string]
	if err := json.Unmarshal([]byte(`{"none":true}`), &a); err != nil || !a.None || a.Value != nil {
		t.Fatalf("none: %+v %v", a, err)
	}
	if err := json.Unmarshal([]byte(`{"value":"7"}`), &a); err != nil || a.None || a.Value == nil || *a.Value != "7" {
		t.Fatalf("value: %+v %v", a, err)
	}
	for _, bad := range []string{`{}`, `{"none":false}`, `{"none":true,"value":"1"}`, `{"value":"1","extra":1}`, `{"none":"yes"}`, `"x"`, `5`} {
		var x Attested[string]
		if err := json.Unmarshal([]byte(bad), &x); err == nil {
			t.Errorf("%s must be rejected", bad)
		}
	}
	out, _ := json.Marshal(Attested[int64]{None: true})
	if string(out) != `{"none":true}` {
		t.Errorf("none marshals as %s", out)
	}
	v := int64(5)
	out, _ = json.Marshal(Attested[int64]{Value: &v})
	if string(out) != `{"value":5}` {
		t.Errorf("value marshals as %s", out)
	}
}

// ---- SQLSTATE mapping -----------------------------------------------------------------------

func TestMapDBError(t *testing.T) {
	pg := func(code, msg, detail string) error { return &pgconn.PgError{Code: code, Message: msg, Detail: detail} }
	cases := []struct {
		err    error
		code   RefusalCode
		detail string
	}{
		{pg("RL003", "", ""), RefusedBackdated, ""},
		{pg("RL004", "", "field=ceiling parent=abc"), RefusedOperatorLoosens, "ceiling"},
		{pg("RL004", "", "garbage"), RefusedOperatorLoosens, ""},
		{pg("RL005", "", ""), RefusedNoFloor, ""},
		{pg("RL006", "", ""), RefusedGuard, "floor_without_approved_request"},
		{pg("RL007", "", ""), RefusedSelfApproval, ""},
		{pg("RL008", "", ""), RefusedSyntheticRefused, ""},
		{pg("RL009", "", ""), RefusedGuard, "RL009"},
		{pg("RL010", "", ""), RefusedRequestNotPending, ""},
		{pg("RL011", "", ""), RefusedInvalid, "calendar_tz is not a known time zone"},
		{pg("RL020", "", ""), RefusedGuard, "RL020"},
		{pg("RL021", "", ""), RefusedNoChange, ""},
		{pg("RL022", "the cooling-off period runs until x", ""), RefusedCoolingOff, ""},
		{pg("RL022", "not the latest request", ""), RefusedChangeNotPending, ""},
		{pg("RL023", "", ""), RefusedConfigMissing, ""},
		{pg("RL024", "", ""), RefusedUnavailable, "isolation"},
		{pg("RL025", "staff may not change their own Person's limits", ""), RefusedSelfTarget, ""},
		{pg("RL025", "the row's tenant is not the session's tenant", ""), RefusedForbidden, ""},
		{pg("RL001", "", ""), RefusedForbidden, ""},
		{pg("RL026", "", ""), RefusedGuard, "RL026"},
		{pg("AP002", "", ""), RefusedProof, "AP002"},
		{pg("AP004", "", ""), RefusedProof, "AP004"},
		{pg("55P03", "", ""), RefusedUnavailable, "busy"},
		{pg("40P01", "", ""), RefusedUnavailable, "busy"},
		{pg("23514", "", ""), RefusedInvalid, "the request violates a configuration shape rule"},
		{pg("23503", "", ""), RefusedInvalid, "a referenced asset, brand or jurisdiction does not exist"},
		{pg("22P02", "", ""), RefusedInvalid, "a value is malformed"},
	}
	for _, c := range cases {
		got := MapDBError(c.err)
		r, ok := IsRefusal(got)
		if !ok || r.Code != c.code || r.Detail != c.detail {
			t.Errorf("%v: got %v, want %s/%q", c.err, got, c.code, c.detail)
		}
		wrapped := ClassifyWrap(c.err)
		if r2, ok := IsRefusal(MapDBError(wrapped)); !ok || r2.Code != c.code {
			t.Errorf("a wrapped %v must map identically, got %v", c.err, wrapped)
		}
	}
	// no leak of the database text through a refusal
	for _, c := range cases {
		r, _ := IsRefusal(MapDBError(pg(c.err.(*pgconn.PgError).Code, "secret table name", "secret detail")))
		if strings.Contains(r.Error(), "secret") {
			t.Errorf("%v: refusal text leaks database text: %q", c.err, r.Error())
		}
	}
	plain := errors.New("boom")
	if MapDBError(plain) != plain {
		t.Error("an unrecognised error is returned unchanged (treated as internal)")
	}
	if MapDBError(nil) != nil {
		t.Error("nil stays nil")
	}
	if _, ok := IsRefusal(MapDBError(&pgconn.PgError{Code: "XX000"})); ok {
		t.Error("an unknown SQLSTATE must not be turned into a refusal")
	}
}

// ClassifyWrap wraps an error the way the service wraps database errors.
func ClassifyWrap(err error) error { return errors.Join(ErrRGDatabase, err) }

func TestOpenPendingStatusAndPendingFlag(t *testing.T) {
	for _, k := range []ChangeKind{ChangeIncrease, ChangeRemoval} {
		if !(LimitChange{ChangeKind: k}).Pending() {
			t.Errorf("%s is a pending-capable change", k)
		}
	}
	for _, k := range []ChangeKind{ChangeInitial, ChangeDecrease, ChangeConfirm, ChangeCancelPending} {
		if (LimitChange{ChangeKind: k}).Pending() {
			t.Errorf("%s takes effect immediately", k)
		}
	}
}

func TestNewLimitsDefaults(t *testing.T) {
	l := NewLimits(LimitsConfig{})
	if l.cfg.PendingCap != DefaultPendingCap || l.cfg.Audit == nil {
		t.Fatalf("defaults: %+v", l.cfg)
	}
	if NewLimits(LimitsConfig{PendingCap: 2}).cfg.PendingCap != 2 {
		t.Fatal("an explicit cap is kept")
	}
	if NewLimits(LimitsConfig{PendingCap: -1}).cfg.PendingCap != DefaultPendingCap {
		t.Fatal("a non-positive cap falls back to the default")
	}
}
