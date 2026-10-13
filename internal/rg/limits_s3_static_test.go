package rg

// ADR 0115 S3 static and unit pins (no database).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestS3RGOnlySpecsArePinned(t *testing.T) {
	// The RG-only specs are closed lists; widening one is a ledger-finance decision, not a drive-by.
	for name, c := range map[string]struct{ got, want []string }{
		"stake types":    {RGStakeTransactionTypes, []string{"casino_bet", "sportsbook_bet"}},
		"reversal types": {RGReversalTransactionTypes, []string{"casino_rollback", "sportsbook_void", "sportsbook_rollback"}},
		"loss types": {RGLossTransactionTypes, []string{"casino_bet", "casino_win", "casino_rollback", "sportsbook_bet",
			"sportsbook_settlement", "sportsbook_void", "sportsbook_rollback"}},
		"cash accounts":   {RGCashAccountTypes, []string{"player_cash"}},
		"bonus accounts":  {RGBonusAccountTypes, []string{"player_bonus", "player_bonus_held"}},
		"ignored (locks)": {RGIgnoredAccountTypes, []string{"player_locked_cash", "player_locked_bonus"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s changed: %v, want %v", name, c.got, c.want)
		}
	}
	// bonus_conversion is not a loss type: a conversion never reduces loss usage (ADR 0115 1.4).
	for _, ty := range RGLossTransactionTypes {
		if ty == "bonus_conversion" || ty == "deposit" || ty == "bonus_reversal" {
			t.Errorf("%s must not be in the loss set", ty)
		}
	}
	// Every reversal type is a loss type (a reversal is netted into loss), and no stake type is a reversal.
	for _, ty := range RGReversalTransactionTypes {
		if !inSet(RGLossTransactionTypes, ty) {
			t.Errorf("reversal type %s must be in the loss set", ty)
		}
		if inSet(RGStakeTransactionTypes, ty) {
			t.Errorf("reversal type %s must not be a stake type", ty)
		}
	}
	// Cash, bonus and ignored account types are pairwise disjoint.
	for _, a := range RGCashAccountTypes {
		if inSet(RGBonusAccountTypes, a) || inSet(RGIgnoredAccountTypes, a) {
			t.Errorf("%s declared twice", a)
		}
	}
	for _, a := range RGBonusAccountTypes {
		if inSet(RGIgnoredAccountTypes, a) {
			t.Errorf("%s declared twice", a)
		}
	}
}

func TestS3WindowSpecValidation(t *testing.T) {
	good := []WindowSpec{{Period: PeriodRolling24h}, {Period: PeriodCalendarDay, CalendarTZ: "UTC"}, {Period: PeriodCalendarWeek, CalendarTZ: "UTC", WeekStart: 7},
		{Period: PeriodCalendarMonth, CalendarTZ: "Europe/Berlin"}}
	if p, tz, ws, err := windowArgs(good); err != nil || len(p) != 4 || tz[0] != "UTC" || ws[0] != 1 || ws[2] != 7 || p[3] != "calendar_month" {
		t.Fatalf("good specs: %v %v %v %v", p, tz, ws, err)
	}
	for _, bad := range [][]WindowSpec{
		nil, {}, {{Period: "hourly"}}, {{Period: PeriodCalendarDay}}, {{Period: PeriodCalendarDay, CalendarTZ: strings.Repeat("x", 65)}},
		{{Period: PeriodCalendarWeek, CalendarTZ: "UTC"}}, {{Period: PeriodCalendarWeek, CalendarTZ: "UTC", WeekStart: 8}},
		make([]WindowSpec, MaxUsageWindows+1),
	} {
		if _, _, _, err := windowArgs(bad); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
}

func TestS3UsageRequestValidation(t *testing.T) {
	ok := UsageRequest{TenantID: uuid.New(), PlayerAccountID: uuid.New(), Scope: LimitScopeTenantPerson, AssetCode: "USD"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*UsageRequest){
		func(r *UsageRequest) { r.TenantID = uuid.Nil }, func(r *UsageRequest) { r.PlayerAccountID = uuid.Nil },
		func(r *UsageRequest) { r.Scope = LimitScopePlatformPerson }, func(r *UsageRequest) { r.Scope = "" },
		func(r *UsageRequest) { r.AssetCode = "" }, func(r *UsageRequest) { r.AssetCode = strings.Repeat("A", 33) },
	} {
		r := ok
		mut(&r)
		if err := r.validate(); err == nil {
			t.Errorf("%+v must be refused", r)
		}
	}
}

func TestS3FloorLossAndParseMinor(t *testing.T) {
	neg := big.NewInt(-5)
	if got := floorLoss(neg, false); got.Sign() != 0 {
		t.Errorf("floor 0 expected, got %s", got)
	}
	if got := floorLoss(neg, true); got.Cmp(neg) != 0 {
		t.Errorf("attested negative expected, got %s", got)
	}
	if got := floorLoss(big.NewInt(7), false); got.Int64() != 7 {
		t.Errorf("positive loss unchanged, got %s", got)
	}
	// The floored value is a copy, not an alias of the raw figure.
	raw := big.NewInt(9)
	cp := floorLoss(raw, true)
	cp.Add(cp, big.NewInt(1))
	if raw.Int64() != 9 {
		t.Error("floorLoss must not alias its input")
	}
	s := "99999999999999999999999999999999999999"
	if v, err := parseMinor(&s); err != nil || v.String() != s {
		t.Errorf("38-digit value: %v %v", v, err)
	}
	if v, err := parseMinor(nil); err != nil || v.Sign() != 0 {
		t.Errorf("nil is zero: %v %v", v, err)
	}
	bad := "1.5"
	if _, err := parseMinor(&bad); err == nil {
		t.Error("a fractional minor-unit value must be refused")
	}
}

func TestS3CreditKind(t *testing.T) {
	player, other := uuid.New(), uuid.New()
	tx := uuid.New()
	dep, wrong, cr := "deposit", "casino_win", "300"
	short := "250"
	cases := []struct {
		name                  string
		status, amount, asset string
		txID                  *uuid.UUID
		txType, credited      *string
		accounts              []uuid.UUID
		assets                []string
		want                  CreditMismatchKind
	}{
		{"match", "succeeded", "300", "USD", &tx, &dep, &cr, []uuid.UUID{player}, []string{"USD"}, ""},
		{"pending no posting", "pending", "300", "USD", nil, nil, nil, nil, nil, ""},
		{"succeeded no posting", "succeeded", "300", "USD", nil, nil, nil, nil, nil, MismatchSucceededWithoutPosting},
		{"posting on failed", "failed", "300", "USD", &tx, &dep, &cr, []uuid.UUID{player}, []string{"USD"}, MismatchPostingOnUnsettledIntent},
		{"wrong type", "succeeded", "300", "USD", &tx, &wrong, &cr, []uuid.UUID{player}, []string{"USD"}, MismatchWrongPostingType},
		{"amount", "succeeded", "300", "USD", &tx, &dep, &short, []uuid.UUID{player}, []string{"USD"}, MismatchAmount},
		{"no credit leg", "succeeded", "300", "USD", &tx, &dep, nil, nil, nil, MismatchAmount},
		{"account", "succeeded", "300", "USD", &tx, &dep, &cr, []uuid.UUID{other}, []string{"USD"}, MismatchAccount},
		{"two accounts", "succeeded", "300", "USD", &tx, &dep, &cr, []uuid.UUID{player, other}, []string{"USD"}, MismatchAccount},
		{"asset", "succeeded", "300", "USD", &tx, &dep, &cr, []uuid.UUID{player}, []string{"EUR"}, MismatchAsset},
	}
	for _, c := range cases {
		if got := creditKind(c.status, c.amount, c.asset, player, c.txID, c.txType, c.credited, c.accounts, c.assets); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestS3QueryShape(t *testing.T) {
	for n := 1; n <= MaxUsageWindows; n++ {
		for _, person := range []bool{false, true} {
			for name, q := range map[string]string{"stake": stakeUsageSQL(n, person), "deposit": depositUsageSQL(n, person)} {
				if strings.Count(q, "clock_timestamp()") != 1 {
					t.Errorf("%s n=%d: the statement instant must be read exactly once", name, n)
				}
				if !strings.Contains(q, "tenant_id = $1") || !strings.Contains(q, "FILTER (WHERE") {
					t.Errorf("%s n=%d: tenant predicate / FILTER windows missing", name, n)
				}
				for _, banned := range []string{"float", "double precision", "real)", "now()", "interval '1 day' *", "'1 day'::"} {
					if strings.Contains(q, banned) {
						t.Errorf("%s n=%d contains %q", name, n, banned)
					}
				}
				if strings.Contains(q, "%!") || strings.Contains(q, "(MISSING)") || strings.Contains(q, "EXTRA") {
					t.Errorf("%s n=%d: formatting error in the SQL", name, n)
				}
			}
		}
	}
	// Rolling windows use fixed hour intervals, never '1 day' (DST-sensitive); the only day arithmetic is on a
	// local timestamp WITHOUT time zone (calendar week).
	e := windowStartExpr(1, 5, 6, 7)
	for _, want := range []string{"interval '24 hours'", "interval '168 hours'", "interval '720 hours'", "date_trunc('day'", "date_trunc('month'", "AT TIME ZONE"} {
		if !strings.Contains(e, want) {
			t.Errorf("window expression lacks %q", want)
		}
	}
}

// TestS3NoMoneyFloatNoCacheNoWiring pins the money rules of the new files and the S3 scope boundary: the
// measurement is not called from wallet, payments, casino, sportsbook or any other package (S4 wires it).
func TestS3NoMoneyFloatNoCacheNoWiring(t *testing.T) {
	files, err := filepath.Glob("limits_usage*.go")
	if err != nil || len(files) < 4 {
		t.Fatalf("usage files: %v %v", files, err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, "redis") || strings.Contains(p, "cache") {
				t.Errorf("%s imports %s: no cache on the money path", f, p)
			}
		}
		full, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(full, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && (id.Name == "float64" || id.Name == "float32") {
				t.Errorf("%s: %s at %v - no floating point for money", f, id.Name, fset.Position(id.Pos()))
			}
			return true
		})
	}
	needles := []string{"MeasureStakeUsage", "MeasureDepositUsage", "MeasureSessionUsage", "ReadEffectiveLimitInputs", "CheckDepositCredits", "WindowStarts"}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if rel, _ := filepath.Rel(filepath.Join(root, "internal", "rg"), path); !strings.HasPrefix(rel, "..") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, n := range needles {
				if strings.Contains(string(b), "rg."+n) {
					t.Errorf("%s calls rg.%s: ADR 0115 S3 is measurement only; wiring it into a money path is S4", path, n)
				}
			}
			return nil
		})
	}
}
