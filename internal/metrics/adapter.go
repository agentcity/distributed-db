package metrics

import "time"

// Done completes one tracked operation. It may be called at most once
// meaningfully; SLO.Begin's implementation ignores repeated calls.
type Done func(err error)

// Recorder is the minimal instrumentation hook a request-serving package
// (KV RPC, SQL executor, Raft transport) can accept without depending on a
// concrete metrics implementation. Implementations must be safe for concurrent
// use; *SLO implements it.
type Recorder interface {
	ObserveRequest(latency time.Duration, err error)
}

// Tracker adds in-flight accounting to Recorder. *SLO implements it.
type Tracker interface {
	Recorder
	Begin() Done
}

// NopRecorder is a Recorder and Tracker that does nothing. It lets callers
// default to instrumentation being disabled without nil checks.
type NopRecorder struct{}

// ObserveRequest implements Recorder.
func (NopRecorder) ObserveRequest(time.Duration, error) {}

// Begin implements Tracker.
func (NopRecorder) Begin() Done { return func(error) {} }

// RecorderFunc adapts a function to Recorder. A nil RecorderFunc is a no-op.
type RecorderFunc func(latency time.Duration, err error)

// ObserveRequest implements Recorder.
func (f RecorderFunc) ObserveRequest(latency time.Duration, err error) {
	if f != nil {
		f(latency, err)
	}
}

// Observe runs fn, measures its latency, and records the result through rec.
// A nil rec disables recording. The original error is returned unchanged. If
// fn panics the observation is not recorded and the panic propagates.
func Observe(rec Recorder, fn func() error) error {
	start := time.Now()
	err := fn()
	if rec != nil {
		rec.ObserveRequest(time.Since(start), err)
	}
	return err
}

// Track begins an operation, runs fn, and completes it with fn's error. Unlike
// Observe it also maintains in-flight accounting. A nil tracker just runs fn.
// The deferred completion guarantees in-flight is decremented if fn panics.
func Track(tr Tracker, fn func() error) (err error) {
	if tr == nil {
		return fn()
	}
	done := tr.Begin()
	defer func() { done(err) }()
	return fn()
}
