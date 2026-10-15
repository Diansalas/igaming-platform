package channels_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"
	"github.com/Diansalas/igaming-platform/internal/alerting/channels"
	"github.com/Diansalas/igaming-platform/internal/email"
)

// scriptedProvider is a synthetic (MOCK-marked) provider with scripted failures and a hang mode.
type scriptedProvider struct {
	mu     sync.Mutex
	sent   []email.Message
	failTo map[string]error
	hang   bool
	global error
}

func (*scriptedProvider) SyntheticComponent() {}

func (p *scriptedProvider) Send(ctx context.Context, m email.Message) error {
	p.mu.Lock()
	hang, ge, fe := p.hang, p.global, p.failTo[m.To]
	p.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if ge != nil {
		return ge
	}
	if fe != nil {
		return fe
	}
	p.mu.Lock()
	p.sent = append(p.sent, m)
	p.mu.Unlock()
	return nil
}

func (p *scriptedProvider) messages() []email.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]email.Message(nil), p.sent...)
}

// nonSynthetic has no Synthetic marker: it stands for a future real vendor adapter.
type nonSynthetic struct {
	mu   sync.Mutex
	sent []email.Message
}

// MarkProductionEligible is the positive marker a real adapter carries (providerkind.ProductionEligible).
func (p *nonSynthetic) MarkProductionEligible() {}

func (p *nonSynthetic) Send(_ context.Context, m email.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, m)
	return nil
}

func TestEmailSink_Conformance(t *testing.T) {
	alertingtest.RunChannelConformance(t, func(t *testing.T, logger *slog.Logger) alertingtest.Subject {
		p := &scriptedProvider{}
		s, err := channels.NewEmailSink(p, []string{"ops@example.com"}, "development", logger)
		if err != nil {
			t.Fatal(err)
		}
		return alertingtest.Subject{
			Sink:          s,
			Notifications: func() int { return len(p.messages()) },
			Hang:          func() { p.mu.Lock(); p.hang = true; p.mu.Unlock() },
			Fail: func(c alerting.ErrorClass) {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.global = nil
				if c == alerting.ErrorClassTimeout {
					p.global = context.DeadlineExceeded
				} else if c != alerting.ErrorClassNone {
					p.global = errors.New("provider failure")
				}
			},
			// The email sink classifies a provider error as unavailable and a deadline as timeout; the permanent
			// classes (rejected/misconfigured) come from the provider only through a typed error it does not have.
			FailClasses: []alerting.ErrorClass{alerting.ErrorClassTimeout, alerting.ErrorClassUnavailable},
		}
	})
}

func TestEmailSink_MockProviderIsNeverAHumanNotification(t *testing.T) {
	s, err := channels.NewEmailSink(email.NewMockProvider(), []string{"ops@example.com"}, "development", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.HumanNotification() {
		t.Fatal("an email channel over the MOCK provider reached no person and must say so")
	}
	if s.ChannelKind() != alerting.ChannelEmail {
		t.Fatal("wrong channel kind")
	}
}

func TestEmailSink_ProductionRefusesAMockProvider(t *testing.T) {
	for _, env := range []string{"production"} {
		if _, err := channels.NewEmailSink(email.NewMockProvider(), []string{"ops@example.com"}, env, nil); err == nil {
			t.Errorf("%s accepted the MOCK email provider", env)
		}
		if _, err := channels.NewEmailSink(&scriptedProvider{}, []string{"ops@example.com"}, env, nil); err == nil {
			t.Errorf("%s accepted a synthetic provider", env)
		}
	}
	for _, env := range []string{"development", "staging"} {
		if _, err := channels.NewEmailSink(email.NewMockProvider(), []string{"ops@example.com"}, env, nil); err != nil {
			t.Errorf("%s refused the MOCK provider: %v", env, err)
		}
	}
	// An UNMARKED provider (neither synthetic nor production-eligible) is refused too: fail closed.
	if _, err := channels.NewEmailSink(unmarked{}, []string{"ops@example.com"}, "production", nil); err == nil {
		t.Error("production accepted a provider that is not marked production-eligible")
	}
	// A production-eligible provider is accepted in production and IS a human notification.
	s, err := channels.NewEmailSink(&nonSynthetic{}, []string{"ops@example.com"}, "production", nil)
	if err != nil {
		t.Fatalf("production refused a non-synthetic provider: %v", err)
	}
	if !s.HumanNotification() {
		t.Fatal("an email channel over a real provider is a human notification")
	}
}

func TestEmailSink_ConstructionIsFailClosed(t *testing.T) {
	good := []string{"ops@example.com"}
	if _, err := channels.NewEmailSink(nil, good, "development", nil); err == nil {
		t.Error("nil provider accepted")
	}
	if _, err := channels.NewEmailSink(&nonSynthetic{}, nil, "development", nil); err == nil {
		t.Error("a sink with no recipients was built")
	}
	for _, bad := range []string{"", "a@b.c\r\nBcc: x@y.z", "a@b.c,d@e.f", "Name <a@b.c>", "a b@c.d"} {
		if _, err := channels.NewEmailSink(&nonSynthetic{}, []string{bad}, "development", nil); err == nil {
			t.Errorf("recipient %q accepted", bad)
		}
	}
}

func TestEmailSink_MessageHasIdentifiersOnly(t *testing.T) {
	p := &scriptedProvider{}
	s, _ := channels.NewEmailSink(p, []string{"ops@example.com", "second@example.com"}, "development", nil)
	d := sampleDelivery()
	d.Discriminator = "x\r\nBcc: attacker@example.com"
	if out, _ := s.Deliver(context.Background(), d); out != alerting.OutcomeSent {
		t.Fatal("not sent")
	}
	msgs := p.messages()
	if len(msgs) != 2 || msgs[0].To != "ops@example.com" || msgs[1].To != "second@example.com" {
		t.Fatalf("messages = %+v", msgs)
	}
	for _, m := range msgs {
		if strings.ContainsAny(m.Subject, "\r\n") {
			t.Errorf("subject has a line break: %q", m.Subject)
		}
		if strings.Contains(m.Subject+m.Body, "attacker@example.com\n") || strings.Contains(m.Body, "Bcc: attacker") {
			t.Errorf("header/line injection through the discriminator: %q", m.Body)
		}
		for _, bad := range []string{"CANARY-PII-7731", "alice@example.com", "operator"} {
			if strings.Contains(m.Subject+m.Body, bad) {
				t.Errorf("message leaks %q", bad)
			}
		}
		for _, want := range []string{"payment.webhook_integrity", "P1", d.AlertID.String(), d.SubjectTenantID.String(), "observability-and-alerting.md"} {
			if !strings.Contains(m.Subject+m.Body, want) {
				t.Errorf("message lacks %q:\n%s", want, m.Body)
			}
		}
	}
	// a subjectless alert (platform Kind) does not print a nil tenant
	d2 := sampleDelivery()
	d2.SubjectTenantID = uuid.Nil
	d2.Kind = alerting.KindReconciliationSweepFailed
	s.Deliver(context.Background(), d2)
	last := p.messages()[len(p.messages())-1]
	if strings.Contains(last.Body, "00000000-0000-0000-0000-000000000000") {
		t.Errorf("a nil tenant id was printed:\n%s", last.Body)
	}
}

func TestEmailSink_PartialFailureRetriesOnlyTheRecipientsThatFailed(t *testing.T) {
	p := &scriptedProvider{failTo: map[string]error{"second@example.com": errors.New("mailbox unavailable")}}
	s, _ := channels.NewEmailSink(p, []string{"ops@example.com", "second@example.com"}, "development", nil)
	d := sampleDelivery()
	out, c := s.Deliver(context.Background(), d)
	if out != alerting.OutcomeFailed || c != alerting.ErrorClassUnavailable {
		t.Fatalf("partial failure = %q/%q, want failed/unavailable", out, c)
	}
	p.mu.Lock()
	p.failTo = nil
	p.mu.Unlock()
	if out, _ := s.Deliver(context.Background(), d); out != alerting.OutcomeSent {
		t.Fatal("retry did not complete")
	}
	var first, second int
	for _, m := range p.messages() {
		switch m.To {
		case "ops@example.com":
			first++
		case "second@example.com":
			second++
		}
	}
	if first != 1 || second != 1 {
		t.Fatalf("sends: first=%d second=%d, want exactly one each (the recipient that succeeded is not re-notified)", first, second)
	}
	// and a fully delivered key is not sent again
	s.Deliver(context.Background(), d)
	if len(p.messages()) != 2 {
		t.Fatalf("a delivered dedup key was re-sent: %d messages", len(p.messages()))
	}
}

func TestEmailSink_NeverLogsBodyOrRecipient(t *testing.T) {
	var sb strings.Builder
	var mu sync.Mutex
	w := logWriter(func(b []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return sb.Write(b) })
	p := &scriptedProvider{global: errors.New("smtp rejected-ZXQ9KT ops@example.com")}
	s, _ := channels.NewEmailSink(p, []string{"ops@example.com"}, "development", slog.New(slog.NewTextHandler(w, nil)))
	s.Deliver(context.Background(), sampleDelivery())
	mu.Lock()
	defer mu.Unlock()
	for _, bad := range []string{"ops@example.com", "ZXQ9KT", "CANARY-PII-7731", "Alert id"} {
		if strings.Contains(sb.String(), bad) {
			t.Errorf("log leaks %q: %s", bad, sb.String())
		}
	}
}

func TestEmailSink_HungProviderIsBoundedByTheContext(t *testing.T) {
	p := &scriptedProvider{hang: true}
	s, _ := channels.NewEmailSink(p, []string{"ops@example.com"}, "development", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, c := s.Deliver(ctx, sampleDelivery())
	if out != alerting.OutcomeFailed || c != alerting.ErrorClassTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("hung provider => %q/%q after %v", out, c, time.Since(start))
	}
}

func TestEmailSink_ProviderPanicIsAFailureNotACrash(t *testing.T) {
	s, _ := channels.NewEmailSink(panicProvider{}, []string{"ops@example.com"}, "development", nil)
	out, c := s.Deliver(context.Background(), sampleDelivery())
	if out != alerting.OutcomeFailed || c == alerting.ErrorClassNone {
		t.Fatalf("panicking provider => %q/%q", out, c)
	}
}

type panicProvider struct{}

func (panicProvider) SyntheticComponent()                       {}
func (panicProvider) Send(context.Context, email.Message) error { panic("provider bug") }

type unmarked struct{}

func (unmarked) Send(context.Context, email.Message) error { return nil }
