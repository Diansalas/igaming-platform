package httpserver

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payments"
)

// SA-5 (docs/plans/staging-acceptance-suite-record.md): the payment capability PUT states the EFFECTIVE stored
// configuration and whether it differs from the request. DB-free parts.

func storedCapability(mutate func(*payments.ProviderCapability)) payments.ProviderCapability {
	pc := payments.ProviderCapability{
		AdapterCapability: payments.AdapterCapability{
			SupportedFiatCurrencies: []string{"EUR", "USD"},
			SupportedPaymentMethods: []string{"card", "bank_transfer"},
			SupportsDeposit:         true, SupportsWithdrawal: true,
			AmountLimits: []payments.AmountLimit{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 500000}},
		},
		Priority: 1, Status: payments.CapabilityActive,
	}
	if mutate != nil {
		mutate(&pc)
	}
	return pc
}

func matchingRequest() writeCapabilityRequest {
	return writeCapabilityRequest{
		SupportedFiatCurrencies: []string{"USD", "EUR"}, // order is not significant
		SupportedPaymentMethods: []string{"bank_transfer", "card"},
		SupportsDeposit:         true, SupportsWithdrawal: true,
		AmountLimits: []amountLimitRequest{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 500000}},
		Priority:     1, Status: "active",
	}
}

func TestCapabilityNarrowed_DetectsEveryDivergenceAndIgnoresOrderAndNilVsEmpty(t *testing.T) {
	if capabilityNarrowed(matchingRequest(), storedCapability(nil)) {
		t.Fatal("an identical configuration (different list order, nil versus empty lists) must not be narrowed")
	}
	cases := map[string]func(*payments.ProviderCapability){
		"a payment method dropped":  func(p *payments.ProviderCapability) { p.SupportedPaymentMethods = []string{"card"} },
		"a currency dropped":        func(p *payments.ProviderCapability) { p.SupportedFiatCurrencies = []string{"EUR"} },
		"a crypto asset added":      func(p *payments.ProviderCapability) { p.SupportedCryptoAssets = []string{"BTC"} },
		"a country added":           func(p *payments.ProviderCapability) { p.SupportedCountries = []string{"MT"} },
		"deposit flipped":           func(p *payments.ProviderCapability) { p.SupportsDeposit = false },
		"withdrawal flipped":        func(p *payments.ProviderCapability) { p.SupportsWithdrawal = false },
		"refund flipped":            func(p *payments.ProviderCapability) { p.SupportsRefundReversal = true },
		"priority changed":          func(p *payments.ProviderCapability) { p.Priority = 2 },
		"status changed":            func(p *payments.ProviderCapability) { p.Status = payments.CapabilityDisabled },
		"a limit dropped":           func(p *payments.ProviderCapability) { p.AmountLimits = nil },
		"a limit max changed":       func(p *payments.ProviderCapability) { p.AmountLimits[0].MaxAmount = 1 },
		"a limit min changed":       func(p *payments.ProviderCapability) { p.AmountLimits[0].MinAmount = 1 },
		"a limit for another asset": func(p *payments.ProviderCapability) { p.AmountLimits[0].AssetCode = "USD" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if !capabilityNarrowed(matchingRequest(), storedCapability(mutate)) {
				t.Fatalf("%s: must be reported as narrowed", name)
			}
		})
	}
	// A request that asks for something the store does not have (the other direction).
	req := matchingRequest()
	req.SupportedPaymentMethods = append(req.SupportedPaymentMethods, "sepa")
	if !capabilityNarrowed(req, storedCapability(nil)) {
		t.Fatal("a requested method that was not stored must be reported as narrowed")
	}
}

func TestToWriteCapabilityResponse_EveryListIsPresentAndLimitsAreOrdered(t *testing.T) {
	id := uuid.New()
	resp := toWriteCapabilityResponse(id, matchingRequest(), storedCapability(func(p *payments.ProviderCapability) {
		p.AmountLimits = []payments.AmountLimit{{AssetCode: "USD", MinAmount: 1, MaxAmount: 2}, {AssetCode: "EUR", MinAmount: 3, MaxAmount: 4}}
	}))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	eff := m["effective"].(map[string]any)
	for _, k := range []string{"supported_fiat_currencies", "supported_crypto_assets", "supported_payment_methods", "supported_countries", "amount_limits"} {
		if _, ok := eff[k].([]any); !ok {
			t.Errorf("%s must be a JSON array (never null): %v", k, eff[k])
		}
	}
	// The request asked for one limit, the store holds two: the response must say it differs.
	if resp.ID != id.String() || !resp.Narrowed {
		t.Errorf("id/narrowed = %s/%v, want the divergence reported", resp.ID, resp.Narrowed)
	}
	if got := resp.Effective.AmountLimits; len(got) != 2 || got[0].AssetCode != "EUR" || got[1].AssetCode != "USD" {
		t.Errorf("amount limits must be ordered by asset: %+v", got)
	}
}

func TestToWriteCapabilityResponse_NarrowedFlagFollowsTheComparison(t *testing.T) {
	same := toWriteCapabilityResponse(uuid.New(), matchingRequest(), storedCapability(nil))
	if same.Narrowed {
		t.Error("an identical configuration must not be reported as narrowed")
	}
	dropped := toWriteCapabilityResponse(uuid.New(), matchingRequest(), storedCapability(func(p *payments.ProviderCapability) {
		p.SupportedPaymentMethods = []string{"card"}
	}))
	if !dropped.Narrowed || len(dropped.Effective.SupportedPaymentMethods) != 1 {
		t.Errorf("a dropped method must be reported as narrowed and the effective list must show it: %+v", dropped)
	}
}

func TestOpenAPI_PaymentCapabilityResponse_MatchesDTO(t *testing.T) {
	spec := launchSpec(t)
	block := rgSchemaBlock(t, spec, "WriteProviderCapabilityResponse")
	for _, tag := range rgJSONTags(writeCapabilityResponse{}) {
		if !regexp.MustCompile(`(?m)^        ` + tag + `:`).MatchString(block) {
			t.Errorf("response field %q is not documented", tag)
		}
	}
	// The effective object documents exactly the DTO's fields.
	for _, tag := range rgJSONTags(capabilityEffectiveResponse{}) {
		if !regexp.MustCompile(`(?m)^            ` + tag + `:`).MatchString(block) {
			t.Errorf("effective field %q is not documented", tag)
		}
	}
	documented := regexp.MustCompile(`(?m)^            ([a-z_]+):`).FindAllStringSubmatch(block, -1)
	want := map[string]bool{}
	for _, tag := range rgJSONTags(capabilityEffectiveResponse{}) {
		want[tag] = true
	}
	for _, d := range documented {
		// the nested amount-limit properties sit deeper (18 spaces), so every 12-space key is an effective field
		if !want[d[1]] {
			t.Errorf("documented effective field %q is not in the DTO", d[1])
		}
	}
	if !reflect.DeepEqual(len(documented), len(want)) {
		t.Errorf("documented %d effective fields, DTO has %d", len(documented), len(want))
	}
	op := openAPIPathBlock(t, spec, "/v1/admin/providers/{providerID}/capability")
	for _, want := range []string{"WriteProviderCapabilityResponse", "is refused 400", "never silently reduced", "narrowed"} {
		if !strings.Contains(op, want) {
			t.Errorf("the operation description lacks %q", want)
		}
	}
}
