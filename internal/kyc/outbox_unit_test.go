package kyc

// PRH-2 E1 pure unit tests (no database, no build tag): the derived staff
// submission state precedence (identity-compliance R1/R2), the alert
// discriminator closed set (test 51, M19), ValidateForLoop's lease inequality
// (F9, test 58), the Go/SQL vocabulary parity of the outbox CHECK lists, and the
// shape of the staff-only derived-state API.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func lr(state OutboxState, class, reason string) *latestRow {
	return &latestRow{state: state, lastErrorClass: class, cancelReason: reason}
}

// R2: the precedence table, including the stranded case.
func TestDeriveSubmissionState_PrecedenceTable_R2(t *testing.T) {
	cases := []struct {
		name        string
		create, sub *latestRow
		replacement bool
		want        SubmissionState
	}{
		{"nothing", nil, nil, false, SubmissionState{State: SubmissionNone}},
		{"create pending", lr(OutboxPending, "", ""), nil, false, SubmissionState{State: SubmissionQueued}},
		{"create claimed", lr(OutboxClaimed, "", ""), nil, false, SubmissionState{State: SubmissionQueued}},
		{"create sent", lr(OutboxSent, "", ""), nil, false, SubmissionState{State: SubmissionSent}},
		{"create failed", lr(OutboxFailedTerminal, "ambiguous", ""), nil, false, SubmissionState{State: SubmissionFailed, LastErrorClass: "ambiguous"}},
		{"failed outranks queued", lr(OutboxSent, "", ""), lr(OutboxFailedTerminal, "apply_conflict", ""), false, SubmissionState{State: SubmissionFailed, LastErrorClass: "apply_conflict"}},
		{"failed submit with a pending create", lr(OutboxPending, "", ""), lr(OutboxFailedTerminal, "not_sent", ""), false, SubmissionState{State: SubmissionFailed, LastErrorClass: "not_sent"}},
		{"queued outranks sent", lr(OutboxSent, "", ""), lr(OutboxPending, "", ""), false, SubmissionState{State: SubmissionQueued}},
		// R2: the stranded case staff MUST see: create sent, latest submit cancelled provider_deconfigured.
		{"R2 stranded: sent + cancelled provider_deconfigured", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "provider_deconfigured"), false,
			SubmissionState{State: SubmissionCancelled, CancelReason: "provider_deconfigured"}},
		{"R2 stranded: sent + cancelled decided_concurrently", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "decided_concurrently"), false,
			SubmissionState{State: SubmissionCancelled, CancelReason: "decided_concurrently"}},
		{"R2 stranded: sent + cancelled verification_terminal", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "verification_terminal"), false,
			SubmissionState{State: SubmissionCancelled, CancelReason: "verification_terminal"}},
		{"R2 stranded: sent + cancelled verification_not_submitted", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "verification_not_submitted"), false,
			SubmissionState{State: SubmissionCancelled, CancelReason: "verification_not_submitted"}},
		{"create cancelled provider_deconfigured", lr(OutboxCancelled, "", "provider_deconfigured"), nil, false,
			SubmissionState{State: SubmissionCancelled, CancelReason: "provider_deconfigured"}},
		// Benign cancels do not strand anything.
		{"benign superseded keeps sent", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "superseded"), false, SubmissionState{State: SubmissionSent}},
		{"benign no_documents keeps sent", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "no_documents"), false, SubmissionState{State: SubmissionSent}},
		{"document_set_changed WITH a replacement is benign", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "document_set_changed"), true, SubmissionState{State: SubmissionSent}},
		{"document_set_changed WITHOUT a replacement is shown", lr(OutboxSent, "", ""), lr(OutboxCancelled, "", "document_set_changed"), false,
			SubmissionState{State: SubmissionCancelled, CancelReason: "document_set_changed"}},
		{"only benign cancels", nil, lr(OutboxCancelled, "", "superseded"), false, SubmissionState{State: SubmissionCancelled, CancelReason: "superseded"}},
	}
	for _, c := range cases {
		if got := deriveSubmissionState(c.create, c.sub, c.replacement); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// 51. The discriminator is a closed set and always fits the alert charset; a
// provider id outside the table CHECK charset falls back to provider_unclassified
// (M19: never the raw provider id).
func TestSubmissionDiscriminator_ClosedSet_51(t *testing.T) {
	cases := []struct {
		op       OutboxOperation
		provider string
		class    OutboxErrorClass
		want     string
	}{
		{OpCreate, "mock", ClassAmbiguous, "create:mock"},
		{OpCreate, "mock", ClassNotSent, "create:mock"},
		{OpSubmit, "mock", ClassLeaseExpired, "submit:mock"},
		{OpCreate, "mock", ClassCredentialBindingMismatch, "create:mock:binding_mismatch"},
		{OpSubmit, "sum-sub_1.v2", ClassApplyConflict, "submit:sum-sub_1.v2:apply_conflict"},
		{OpCreate, "bad provider!", ClassAmbiguous, "create:provider_unclassified"},
		{OpCreate, "", ClassCredentialBindingMismatch, "create:provider_unclassified:binding_mismatch"},
		{OpCreate, strings.Repeat("a", 65), ClassApplyConflict, "create:provider_unclassified:apply_conflict"},
		{OpCreate, "x'; DROP TABLE alerts; --", ClassAmbiguous, "create:provider_unclassified"},
	}
	charset := regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,160}$`)
	for _, c := range cases {
		got := submissionDiscriminator(c.op, c.provider, c.class)
		if got != c.want || !charset.MatchString(got) {
			t.Errorf("discriminator(%s,%q,%s) = %q, want %q (charset ok=%v)", c.op, c.provider, c.class, got, c.want, charset.MatchString(got))
		}
	}
}

// 58 (unit part). F9: the default configuration satisfies the lease inequality;
// the loop refuses a lease below 2*(prepare+resolve+call+phaseC)+5s, a missing
// dependency and nonsense constants.
func TestOutboxWorkerValidateForLoop_LeaseInequality_F9(t *testing.T) {
	c := DefaultOutboxConfig()
	if got, want := c.leaseFloor(), 55*time.Second; got != want {
		t.Fatalf("leaseFloor with the defaults = %s, want %s (2 x (5+5+10+5) + 5)", got, want)
	}
	if c.Lease < c.leaseFloor() {
		t.Fatalf("the default lease %s must satisfy the inequality (floor %s)", c.Lease, c.leaseFloor())
	}
	orch := NewOrchestrator(map[string]KYCProvider{"mock": NewMockKYCProvider()}, nil)
	good := &OutboxWorker{Pool: poolSentinel(), Orchestrator: orch, Outbound: NewMockOutboundResolver(), Config: c}
	if err := good.ValidateForLoop(); err != nil {
		t.Fatalf("the default wiring must validate: %v", err)
	}
	mut := func(f func(*OutboxWorker)) error {
		w := *good
		f(&w)
		return w.ValidateForLoop()
	}
	cases := map[string]func(*OutboxWorker){
		"lease one nanosecond below the floor": func(w *OutboxWorker) { w.Config.Lease = w.Config.leaseFloor() - time.Nanosecond },
		"lease above the DB bound":             func(w *OutboxWorker) { w.Config.Lease = 11 * time.Minute },
		"longer prepare, same lease":           func(w *OutboxWorker) { w.Config.PrepareTimeout = 30 * time.Second },
		"longer call, same lease":              func(w *OutboxWorker) { w.Config.CallTimeout = 60 * time.Second },
		"no pool":                              func(w *OutboxWorker) { w.Pool = nil },
		"no orchestrator":                      func(w *OutboxWorker) { w.Orchestrator = nil },
		"no resolver":                          func(w *OutboxWorker) { w.Outbound = nil },
		"zero backoff":                         func(w *OutboxWorker) { w.Config.BackoffBase = 0 },
		"cap below base":                       func(w *OutboxWorker) { w.Config.BackoffCap = time.Second },
		"no attempts":                          func(w *OutboxWorker) { w.Config.MaxFailedAttempts = 0 },
		"no per-tenant cap":                    func(w *OutboxWorker) { w.Config.PerTenantCap = 0 },
	}
	for name, f := range cases {
		if err := mut(f); err == nil {
			t.Errorf("%s: ValidateForLoop must refuse", name)
		}
	}
	if err := (*OutboxWorker)(nil).ValidateForLoop(); err == nil {
		t.Error("a nil worker must be refused")
	}
	// The inequality is exact: the floor itself is accepted.
	if err := mut(func(w *OutboxWorker) { w.Config.Lease = w.Config.leaseFloor() }); err != nil {
		t.Errorf("a lease equal to the floor must be accepted: %v", err)
	}
}

// The retry budget is a PLACEHOLDER technical bound (HQ-E1-3) and the defaults
// are the ADR's RECOMMENDATIONS, pinned so a silent change is a reviewed one.
func TestDefaultOutboxConfig_PinnedRecommendations(t *testing.T) {
	want := OutboxConfig{
		Lease: 60 * time.Second, PrepareTimeout: 5 * time.Second, ResolveBudget: 5 * time.Second, CallTimeout: 10 * time.Second,
		PhaseCTimeout: 5 * time.Second, PhaseCAttempts: 3, PassItemCap: 100, PerTenantCap: 20,
		BackoffBase: 30 * time.Second, BackoffCap: 30 * time.Minute, MaxFailedAttempts: 8,
	}
	if got := DefaultOutboxConfig(); got != want {
		t.Fatalf("DefaultOutboxConfig = %+v, want %+v", got, want)
	}
}

// The Go vocabularies equal the migration's CHECK lists exactly (a class or
// reason added on one side only is a defect: the DB would refuse it, or the Go
// side would never write it).
func TestOutboxVocabularies_MatchMigration0114(t *testing.T) {
	root := kycRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "migrations", "0114_kyc_submission_outbox.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up := string(b)
	list := func(column string) []string {
		re := regexp.MustCompile(column + `\s+TEXT NULL CHECK \(` + column + ` IN\s*\(([^)]*)\)`)
		m := re.FindStringSubmatch(up)
		if m == nil {
			t.Fatalf("CHECK list for %s not found in the migration", column)
		}
		var out []string
		for _, q := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
			out = append(out, q[1])
		}
		sort.Strings(out)
		return out
	}
	goClasses := []string{string(ClassAmbiguous), string(ClassNotSent), string(ClassLeaseExpired), string(ClassDeferredTenantInactive), string(ClassCredentialBindingMismatch), string(ClassApplyConflict)}
	sort.Strings(goClasses)
	if got := list("last_error_class"); !reflect.DeepEqual(got, goClasses) {
		t.Errorf("last_error_class: migration %v != Go %v", got, goClasses)
	}
	goReasons := []string{string(CancelSuperseded), string(CancelVerificationTerminal), string(CancelNoDocuments), string(CancelDocumentSetChanged),
		string(CancelVerificationNotSubmitted), string(CancelProviderDeconfigured), string(CancelDecidedConcurrently)}
	sort.Strings(goReasons)
	if got := list("cancel_reason"); !reflect.DeepEqual(got, goReasons) {
		t.Errorf("cancel_reason: migration %v != Go %v", got, goReasons)
	}
	m := regexp.MustCompile(`state\s+TEXT NOT NULL DEFAULT 'pending'\s+CHECK \(state IN \(([^)]*)\)`).FindStringSubmatch(up)
	if m == nil {
		t.Fatal("state CHECK list not found")
	}
	var states []string
	for _, q := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
		states = append(states, q[1])
	}
	sort.Strings(states)
	goStates := []string{string(OutboxPending), string(OutboxClaimed), string(OutboxSent), string(OutboxFailedTerminal), string(OutboxCancelled)}
	sort.Strings(goStates)
	if !reflect.DeepEqual(states, goStates) {
		t.Errorf("state: migration %v != Go %v", states, goStates)
	}
}

// poolSentinel returns a non-nil *db.Pool for ValidateForLoop (it only checks
// for nil; no connection is made).
func poolSentinel() *db.Pool { return new(db.Pool) }

// The submission-id helpers produce ascending, distinct, text-ordered ids (the
// order the database trigger enforces).
func TestSortedDocumentIDs_AscendingTextOrder(t *testing.T) {
	var docs []SubmittedDocument
	for i := 0; i < 50; i++ {
		docs = append(docs, SubmittedDocument{DocumentID: uuid.New()})
	}
	ids := sortedDocumentIDs(docs)
	for i := 1; i < len(ids); i++ {
		if !(ids[i].String() > ids[i-1].String()) {
			t.Fatalf("not strictly ascending at %d: %s then %s", i, ids[i-1], ids[i])
		}
	}
}
