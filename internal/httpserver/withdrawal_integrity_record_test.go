package httpserver

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Security C-2: a saturated recorder skips instead of spawning more work.
func TestRecordRequestIntegrityRefusal_SaturatedSkips(t *testing.T) {
	for i := 0; i < cap(integrityRefusalSem); i++ {
		integrityRefusalSem <- struct{}{}
	}
	defer func() {
		for len(integrityRefusalSem) > 0 {
			<-integrityRefusalSem
		}
	}()
	recordRequestIntegrityRefusal(Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)), uuid.New(), uuid.New(), uuid.New(), "r", "seal_invalid")
	time.Sleep(300 * time.Millisecond)
	if len(integrityRefusalSem) != cap(integrityRefusalSem) {
		t.Fatalf("a saturated recorder started work (in-flight %d of %d)", len(integrityRefusalSem), cap(integrityRefusalSem))
	}
}

// Security C-2: a panic inside the recorder is recovered and the slot is released.
func TestRecordRequestIntegrityRefusal_PanicIsRecoveredAndSlotReleased(t *testing.T) {
	recordRequestIntegrityRefusal(Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)), uuid.New(), uuid.New(), uuid.New(), "r", "seal_invalid")
	deadline := time.Now().Add(3 * time.Second)
	for len(integrityRefusalSem) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slot was not released after the recorder panicked")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
