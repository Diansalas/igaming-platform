package eventbus

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestInMemoryBus_PublishInvokesSubscriber(t *testing.T) {
	bus := NewInMemoryBus()
	received := make(chan Event, 1)

	bus.Subscribe("player.registered", func(_ context.Context, e Event) error {
		received <- e
		return nil
	})

	want := Event{EventID: uuid.New(), Type: "player.registered", TenantID: uuid.New(), Payload: []byte(`{"player_id":"p1"}`)}
	if err := bus.Publish(context.Background(), want); err != nil {
		t.Fatalf("unexpected error publishing: %v", err)
	}

	select {
	case got := <-received:
		if got.TenantID != want.TenantID {
			t.Errorf("expected tenant id %q, got %q", want.TenantID, got.TenantID)
		}
	default:
		t.Fatal("expected subscriber to be invoked synchronously")
	}
}

func TestInMemoryBus_NoSubscribersIsNoop(t *testing.T) {
	bus := NewInMemoryBus()
	if err := bus.Publish(context.Background(), Event{Type: "nobody.listens"}); err != nil {
		t.Fatalf("expected no error publishing with no subscribers, got %v", err)
	}
}

func TestInMemoryBus_HandlerErrorPropagates(t *testing.T) {
	bus := NewInMemoryBus()
	wantErr := errors.New("boom")
	bus.Subscribe("thing.happened", func(_ context.Context, _ Event) error {
		return wantErr
	})

	err := bus.Publish(context.Background(), Event{Type: "thing.happened"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected handler error to propagate, got %v", err)
	}
}
