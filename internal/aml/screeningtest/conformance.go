// Package screeningtest is the CONFORMANCE suite every aml.ScreeningProvider adapter must pass (ADR 0116 s4.1, A7). It is a
// test-support package (it imports "testing"): production code must not import it. It checks OUR contract, not any vendor's
// semantics: closed outcome vocabulary, internal consistency of a result, idempotency per key, rejection of malformed
// requests (including a platform-id-shaped subject reference), cancellation never producing a clear, and concurrency safety.
package screeningtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// Subjects names, per scenario, a VendorSubjectRef that makes the adapter under test answer that way (the mock uses magic
// markers; a sandbox adapter supplies the synthetic subject references its sandbox documents). An empty field skips the
// scenario; Clear is required.
type Subjects struct {
	Clear       string
	Sanction    string
	PEP         string
	Confirmed   string
	Unavailable string
}

func hex64(seed string) string {
	s := strings.Repeat(seed, 64)
	return s[:64]
}

func req(key, subject string, lists ...aml.ListType) aml.ScreeningRequest {
	if len(lists) == 0 {
		lists = []aml.ListType{aml.ListSanctions, aml.ListPEP}
	}
	return aml.ScreeningRequest{IdempotencyKey: key, SubjectRef: hex64("ab"), Subject: aml.ScreeningSubject{VendorSubjectRef: subject}, Lists: lists}
}

// consistent asserts the internal consistency a conforming result must have for the requested lists.
func consistent(t *testing.T, what string, in aml.ScreeningRequest, r aml.ScreeningResult) {
	t.Helper()
	asked := map[aml.ListType]bool{}
	for _, l := range in.Lists {
		asked[l] = true
	}
	switch r.Outcome {
	case aml.OutcomeClear:
		if len(r.Matches) != 0 {
			t.Errorf("%s: a clear must carry no match: %+v", what, r)
		}
	case aml.OutcomePotentialMatch, aml.OutcomeConfirmed:
		if len(r.Matches) == 0 {
			t.Errorf("%s: a match outcome must name its match(es): %+v", what, r)
		}
		for _, m := range r.Matches {
			if !asked[m.List] {
				t.Errorf("%s: a match on a list that was not requested: %+v", what, m)
			}
		}
	case aml.OutcomeError:
		// an adapter may report a completed error answer; the orchestrator treats it as not-completed
	default:
		t.Errorf("%s: outcome %q is outside the closed vocabulary", what, r.Outcome)
	}
	if r.ScreenedAt.IsZero() {
		t.Errorf("%s: ScreenedAt must be set", what)
	}
}

// Run executes the suite against fresh providers from newProvider.
func Run(t *testing.T, newProvider func() aml.ScreeningProvider, s Subjects) {
	t.Helper()
	if s.Clear == "" {
		t.Fatal("screeningtest: a Clear subject is required")
	}
	ctx := context.Background()

	t.Run("identity_and_capabilities", func(t *testing.T) {
		p := newProvider()
		if id := p.ID(); id == "" || id != strings.ToLower(id) || len(id) > 64 {
			t.Errorf("provider id %q must be a short lower-case token", id)
		}
		c := p.Capabilities()
		if !c.SupportsSubjectReference {
			t.Error("an adapter must screen by the pseudonymous subject reference (C7)")
		}
		if !c.Sanctions && !c.PEP {
			t.Error("an adapter must support at least one list")
		}
	})

	t.Run("malformed_requests_are_refused", func(t *testing.T) {
		p := newProvider()
		good := req("conf-1", s.Clear)
		for name, mut := range map[string]func(r *aml.ScreeningRequest){
			"empty key":              func(r *aml.ScreeningRequest) { r.IdempotencyKey = "" },
			"key with space":         func(r *aml.ScreeningRequest) { r.IdempotencyKey = "a b" },
			"subject ref too short":  func(r *aml.ScreeningRequest) { r.SubjectRef = "abc" },
			"subject ref uppercase":  func(r *aml.ScreeningRequest) { r.SubjectRef = strings.ToUpper(hex64("ab")) },
			"platform id as subject": func(r *aml.ScreeningRequest) { r.SubjectRef = uuid.NewString() },
			"no lists":               func(r *aml.ScreeningRequest) { r.Lists = nil },
			"unknown list":           func(r *aml.ScreeningRequest) { r.Lists = []aml.ListType{"watchlist"} },
			"duplicate list":         func(r *aml.ScreeningRequest) { r.Lists = []aml.ListType{aml.ListPEP, aml.ListPEP} },
		} {
			r := good
			mut(&r)
			if res, err := p.Screen(ctx, r); err == nil {
				t.Errorf("%s: accepted (%+v)", name, res)
			}
		}
	})

	t.Run("clear", func(t *testing.T) {
		p := newProvider()
		in := req("conf-clear", s.Clear)
		r, err := p.Screen(ctx, in)
		if err != nil || r.Outcome != aml.OutcomeClear {
			t.Fatalf("want clear, got %+v %v", r, err)
		}
		consistent(t, "clear", in, r)
	})

	for name, c := range map[string]struct {
		subject string
		lists   []aml.ListType
		outcome aml.ScreeningOutcome
		list    aml.ListType
	}{
		"sanctions_potential_match": {s.Sanction, []aml.ListType{aml.ListSanctions, aml.ListPEP}, aml.OutcomePotentialMatch, aml.ListSanctions},
		"pep_potential_match":       {s.PEP, []aml.ListType{aml.ListSanctions, aml.ListPEP}, aml.OutcomePotentialMatch, aml.ListPEP},
		"confirmed":                 {s.Confirmed, []aml.ListType{aml.ListSanctions}, aml.OutcomeConfirmed, aml.ListSanctions},
	} {
		if c.subject == "" {
			continue
		}
		c := c
		t.Run(name, func(t *testing.T) {
			p := newProvider()
			in := req("conf-"+name, c.subject, c.lists...)
			r, err := p.Screen(ctx, in)
			if err != nil || r.Outcome != c.outcome {
				t.Fatalf("want %s, got %+v %v", c.outcome, r, err)
			}
			consistent(t, name, in, r)
			found := false
			for _, m := range r.Matches {
				found = found || m.List == c.list
			}
			if !found {
				t.Errorf("%s must match on %s: %+v", name, c.list, r.Matches)
			}
		})
	}

	if s.Unavailable != "" {
		t.Run("unavailable_is_an_error_never_a_clear", func(t *testing.T) {
			p := newProvider()
			r, err := p.Screen(ctx, req("conf-unavail", s.Unavailable))
			if err == nil && r.Outcome == aml.OutcomeClear {
				t.Fatalf("an unavailable subject answered clear: %+v", r)
			}
			if err == nil && r.Outcome != aml.OutcomeError {
				t.Fatalf("an unavailable subject must be an error, got %+v", r)
			}
		})
	}

	t.Run("idempotent_per_key", func(t *testing.T) {
		p := newProvider()
		subject := s.Clear
		if s.Sanction != "" {
			subject = s.Sanction
		}
		in := req("conf-idem", subject)
		first, err1 := p.Screen(ctx, in)
		second, err2 := p.Screen(ctx, in)
		if (err1 == nil) != (err2 == nil) || first.Outcome != second.Outcome || first.ProviderReference != second.ProviderReference || !first.ScreenedAt.Equal(second.ScreenedAt) {
			t.Errorf("same key, same request must answer identically: %+v/%v vs %+v/%v", first, err1, second, err2)
		}
		other := req("conf-idem", s.Clear+"-different")
		other.SubjectRef = hex64("cd")
		if _, err := p.Screen(ctx, other); !errors.Is(err, aml.ErrIdempotencyConflict) {
			t.Errorf("same key, different request must be ErrIdempotencyConflict, got %v", err)
		}
	})

	t.Run("cancelled_context_never_clears", func(t *testing.T) {
		p := newProvider()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		r, err := p.Screen(cctx, req("conf-cancel", s.Clear))
		if err == nil {
			t.Fatalf("a cancelled call must fail, got %+v", r)
		}
		if r.Outcome == aml.OutcomeClear {
			t.Fatalf("a cancelled call answered clear: %+v", r)
		}
	})

	t.Run("deadline_is_honoured", func(t *testing.T) {
		p := newProvider()
		dctx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		time.Sleep(5 * time.Millisecond)
		if r, err := p.Screen(dctx, req("conf-deadline", s.Clear)); err == nil && r.Outcome == aml.OutcomeClear {
			t.Fatalf("an expired deadline answered clear: %+v", r)
		}
	})

	t.Run("concurrent_same_key_one_answer", func(t *testing.T) {
		p := newProvider()
		in := req("conf-conc", s.Clear)
		var wg sync.WaitGroup
		results := make([]aml.ScreeningResult, 16)
		errs := make([]error, 16)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = p.Screen(ctx, in)
			}(i)
		}
		wg.Wait()
		for i := range results {
			if errs[i] != nil || results[i].Outcome != results[0].Outcome || results[i].ProviderReference != results[0].ProviderReference {
				t.Fatalf("concurrent identical requests disagree: %+v/%v vs %+v/%v", results[i], errs[i], results[0], errs[0])
			}
		}
	})
}
