package metrics

import (
	"errors"
	"testing"
	"time"
)

func TestTrackAndObserveAdapters(t *testing.T) {
	reg := NewRegistry()
	slo := NewSLO(reg, SLOConfig{Prefix: "test"})
	boom := errors.New("boom")

	err := Track(slo, func() error {
		if got := slo.Inflight.Value(); got != 1 {
			t.Fatalf("inflight inside Track = %d, want 1", got)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Track error = %v, want %v", err, boom)
	}

	err = Observe(slo, func() error { return nil })
	if err != nil {
		t.Fatalf("Observe error = %v, want nil", err)
	}

	err = Observe(nil, func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Observe(nil) error = %v, want %v", err, boom)
	}
	if got := Track(nil, func() error { return nil }); got != nil {
		t.Fatalf("Track(nil) error = %v, want nil", got)
	}

	report := slo.ReportAt(time.Now())
	if report.Requests != 2 || report.Errors != 1 || report.Inflight != 0 {
		t.Fatalf("report = %+v, want 2 requests, 1 error, 0 inflight", report)
	}
}

func TestNopAndFuncRecorder(t *testing.T) {
	nop := NopRecorder{}
	nop.ObserveRequest(time.Second, nil)
	nop.Begin()(nil)

	var gotLatency time.Duration
	var gotErr error
	rec := RecorderFunc(func(latency time.Duration, err error) {
		gotLatency = latency
		gotErr = err
	})
	boom := errors.New("boom")
	if err := Observe(rec, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Observe error = %v, want %v", err, boom)
	}
	if gotErr != boom {
		t.Fatalf("recorded error = %v, want %v", gotErr, boom)
	}
	if gotLatency < 0 {
		t.Fatalf("recorded latency = %v, want >= 0", gotLatency)
	}
}
