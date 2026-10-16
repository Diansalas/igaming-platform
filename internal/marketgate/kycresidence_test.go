package marketgate

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

func TestKYCResidenceLocator_IsProductionEligibleAndNotSynthetic(t *testing.T) {
	var c any = NewKYCResidenceLocator(nil)
	if _, ok := c.(providerkind.Synthetic); ok {
		t.Fatal("the KYC residence locator must not be Synthetic")
	}
	if _, ok := c.(providerkind.ProductionEligible); !ok {
		t.Fatal("the KYC residence locator must be ProductionEligible")
	}
	// ... and the MOCK static Locator stays refused in production.
	if err := providerkind.RefuseSyntheticInProduction("production", []providerkind.Registration{
		{Domain: "geolocation", Name: "locator", Component: geolocation.NewStaticLocator("MT")},
	}); err == nil {
		t.Fatal("the static MOCK locator must remain refused in production")
	}
	if err := providerkind.RefuseSyntheticInProduction("production", []providerkind.Registration{
		{Domain: "geolocation", Name: "locator", Component: NewKYCResidenceLocator(nil)},
	}); err != nil {
		t.Fatalf("the KYC residence locator must pass the production guard: %v", err)
	}
}

func TestKYCResidenceLocator_NoEvidenceWithoutPoolOrPlayer(t *testing.T) {
	tid, pid := uuid.New(), uuid.New()
	var nilLoc *KYCResidenceLocator
	unbound := NewKYCResidenceLocator(nil)
	bound := &KYCResidenceLocator{}
	_ = bound.bindReader(failingReaderUnit{err: errors.New("must not be called")})
	cases := []struct {
		name string
		loc  *KYCResidenceLocator
		req  geolocation.SignalRequest
	}{
		{"nil receiver", nilLoc, geolocation.SignalRequest{TenantID: tid, PlayerAccountID: pid}},
		{"unbound", unbound, geolocation.SignalRequest{TenantID: tid, PlayerAccountID: pid}},
		{"no player account (registration)", bound, geolocation.SignalRequest{TenantID: tid}},
		{"no tenant", bound, geolocation.SignalRequest{PlayerAccountID: pid}},
	}
	for _, tc := range cases {
		res, err := tc.loc.GetSignal(context.Background(), tc.req)
		if err != nil || res.Outcome == geolocation.SignalResolved || res.CountryCode != "" {
			t.Fatalf("%s: want inconclusive without a country, got %+v err=%v", tc.name, res, err)
		}
		if New(tc.loc).Locate(context.Background(), LocateRequest{TenantID: tc.req.TenantID, PlayerAccountID: tc.req.PlayerAccountID}).Known() {
			t.Fatalf("%s: the gate must see unknown evidence", tc.name)
		}
	}
}

func TestKYCResidenceLocator_BindRejectsNilPoolAndBoundReports(t *testing.T) {
	l := NewKYCResidenceLocator(nil)
	if l.Bound() {
		t.Fatal("a locator without a pool is not bound")
	}
	if err := l.Bind(nil); err == nil {
		t.Fatal("Bind(nil) must fail")
	}
	if l.Bound() {
		t.Fatal("a failed Bind must leave the locator unbound")
	}
	_ = l.bindReader(failingReaderUnit{})
	if !l.Bound() {
		t.Fatal("bound after a reader is attached")
	}
	var nilLoc *KYCResidenceLocator
	if nilLoc.Bound() {
		t.Fatal("nil receiver is not bound")
	}
}

func TestKYCResidenceLocator_ReaderErrorIsAnErrorAndUnknown(t *testing.T) {
	l := &KYCResidenceLocator{}
	_ = l.bindReader(failingReaderUnit{err: errors.New("db down")})
	req := geolocation.SignalRequest{TenantID: uuid.New(), PlayerAccountID: uuid.New()}
	if _, err := l.GetSignal(context.Background(), req); err == nil {
		t.Fatal("a reader error must be returned")
	}
	if New(l).Locate(context.Background(), LocateRequest{TenantID: req.TenantID, PlayerAccountID: req.PlayerAccountID}).Known() {
		t.Fatal("a reader error must be unknown evidence")
	}
}

type failingReaderUnit struct{ err error }

func (f failingReaderUnit) WithTenantReadOnly(context.Context, uuid.UUID, db.TxFunc) error {
	return f.err
}
