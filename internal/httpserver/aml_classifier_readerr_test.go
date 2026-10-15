package httpserver

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// A body that DELIVERS a lawful manual / progress body and then fails mid-read is classified at the ceiling: the partial bytes
// must never be trusted to pick the weaker class (ADR 0116 A7 classifiers, SR7-1 / SR7-5).
func TestAMLClassifiers_ReadErrorIsTheCeiling(t *testing.T) {
	for name, c := range map[string]struct {
		fn   stepUpClassifier
		body string
	}{
		"hold":       {amlHoldPlaceClass, `{"person_id":"p","scope":"withdrawal_and_payout","reason_class":"manual","reason_code":"x"}`},
		"transition": {amlTransitionClass, `{"to":"in_review","reason_code":"x"}`},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(io.MultiReader(strings.NewReader(c.body), iotest.ErrReader(errors.New("connection reset")))))
		if got, err := c.fn(Deps{}, r); err != nil || got != auth.StepUpCritical {
			t.Errorf("%s: a read error must classify critical, got %v %v", name, got, err)
		}
		// Control: the same bytes without the failure keep their weaker class.
		r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
		if got, err := c.fn(Deps{}, r); err != nil || got != auth.StepUpNone {
			t.Errorf("%s control: got %v %v", name, got, err)
		}
	}
}
