package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// ADR 0116 A7 round 3 (decodeJSON trailing content): the AML step-up classifiers and the AML handlers decode through the SAME
// single-value rule, so a body that carries a second JSON value (or garbage) after the first is refused by both: the classifier
// answers the ceiling and the handler answers 400. Trailing whitespace is not content.
func TestAMLBodies_TrailingContentIsRefusedByBothTheClassifierAndTheHandler(t *testing.T) {
	manual := `{"person_id":"p","scope":"withdrawal_and_payout","reason_class":"manual","reason_code":"x"}`
	sanctions := `{"person_id":"p","scope":"all_value_movement","reason_class":"confirmed_sanctions","reason_code":"x"}`
	progress := `{"to":"in_review","reason_code":"x"}`
	closeNo := `{"to":"closed_no_action","reason_code":"x"}`
	type route struct {
		name string
		fn   stepUpClassifier
		dst  func() any
		ok   string // lawful, weaker-class body
		bad  []string
	}
	for _, rt := range []route{
		{"hold", amlHoldPlaceClass, func() any { return &amlHoldPlaceRequest{} }, manual,
			[]string{manual + sanctions, manual + " " + sanctions, manual + "\n" + manual, manual + "x", manual + `,`, manual + `{`, manual + "null"}},
		{"transition", amlTransitionClass, func() any { return &amlTransitionRequest{} }, progress,
			[]string{progress + closeNo, progress + progress, progress + "]", progress + "0"}},
	} {
		// Control: the lawful body, with and without trailing whitespace, keeps its weaker class and decodes.
		for _, body := range []string{rt.ok, rt.ok + "\n", rt.ok + " \t\r\n"} {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if got, err := rt.fn(Deps{}, r); err != nil || got != auth.StepUpNone {
				t.Errorf("%s control %q: got %v %v", rt.name, body, got, err)
			}
			r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if err := decodeJSONStrict(r, rt.dst()); err != nil {
				t.Errorf("%s control %q: the handler must decode it: %v", rt.name, body, err)
			}
		}
		for _, body := range rt.bad {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if got, err := rt.fn(Deps{}, r); err != nil || got != auth.StepUpCritical {
				t.Errorf("%s %q: the classifier must answer the ceiling, got %v %v", rt.name, body, got, err)
			}
			r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if err := decodeJSONStrict(r, rt.dst()); err == nil {
				t.Errorf("%s %q: the handler decode must refuse trailing content", rt.name, body)
			}
		}
	}
}

// Pin: the AML route handlers never fall back to the lenient decodeJSON (a second decoder would let a classifier and a handler
// disagree again).
func TestAMLRoutes_DecodeOnlyThroughTheStrictHelper(t *testing.T) {
	src, err := os.ReadFile("aml_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "decodeJSON(") || !strings.Contains(string(src), "decodeJSONStrict(") {
		t.Fatal("aml_routes.go must decode only through decodeJSONStrict")
	}
}
