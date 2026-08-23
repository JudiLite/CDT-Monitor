package engine

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/notify"
	"github.com/JudiLite/CDT-Monitor/internal/store"
)

func TestRunOnceReportsBusyLease(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first := New(st, nil, notify.New(), slog.Default(), 1)
	second := New(st, nil, notify.New(), slog.Default(), 1)
	if _, err = st.AcquireLease(context.Background(), "monitor", first.owner, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = second.RunOnce(context.Background()); !errors.Is(err, ErrMonitorBusy) {
		t.Fatalf("expected busy lease, got %v", err)
	}
}
