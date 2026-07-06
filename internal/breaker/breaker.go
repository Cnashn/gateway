// Package breaker implements a hand-rolled three-state circuit breaker with
// a sliding-window failure ratio, one instance per upstream.
package breaker

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half-open"
	case Open:
		return "open"
	default:
		return "unknown"
	}
}

var ErrOpen = errors.New("circuit breaker is open")

// Breaker guards a single upstream. Allow reserves permission to send one
// request: it returns an error wrapping ErrOpen when the request must be
// short-circuited, otherwise a done callback that must be called with the
// outcome so half-open probe slots are released.
type Breaker interface {
	Allow() (done func(success bool), err error)
	State() State
}

type OpenError struct {
	RetryAfter time.Duration
}

func (e *OpenError) Error() string { return ErrOpen.Error() }

func (e *OpenError) Unwrap() error { return ErrOpen }

type Config struct {
	FailureThreshold    float64
	WindowSize          int
	OpenDuration        time.Duration
	HalfOpenMaxRequests int
}

type Option func(*CircuitBreaker)

// WithClock injects the time source so unit tests can advance time without
// sleeping.
func WithClock(now func() time.Time) Option {
	return func(b *CircuitBreaker) { b.now = now }
}

func WithLogger(logger *slog.Logger) Option {
	return func(b *CircuitBreaker) { b.logger = logger }
}

// WithOnStateChange registers a hook for the metrics layer. It is called
// while the breaker's lock is held, so it must not call back into the
// breaker.
func WithOnStateChange(fn func(upstream string, from, to State)) Option {
	return func(b *CircuitBreaker) { b.onStateChange = fn }
}

// CircuitBreaker is safe for concurrent use via a single mutex. The critical
// sections are a few comparisons and ring buffer writes (no I/O, no
// allocation), so at gateway request rates lock contention is negligible;
// a lock-free version would add subtle ordering bugs for no measurable win.
type CircuitBreaker struct {
	name          string
	cfg           Config
	logger        *slog.Logger
	now           func() time.Time
	onStateChange func(string, State, State)

	mu       sync.Mutex
	state    State
	gen      uint64
	outcomes []bool // ring buffer of recent outcomes, true = failure
	head     int
	count    int
	failures int
	openedAt time.Time
	probes   int
}

func New(name string, cfg Config, opts ...Option) *CircuitBreaker {
	if cfg.WindowSize < 1 {
		cfg.WindowSize = 1
	}
	if cfg.HalfOpenMaxRequests < 1 {
		cfg.HalfOpenMaxRequests = 1
	}
	b := &CircuitBreaker{
		name:     name,
		cfg:      cfg,
		logger:   slog.Default(),
		now:      time.Now,
		state:    Closed,
		outcomes: make([]bool, cfg.WindowSize),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func (b *CircuitBreaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	return b.state
}

func (b *CircuitBreaker) Allow() (func(success bool), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()

	switch b.state {
	case Open:
		remaining := b.cfg.OpenDuration - b.now().Sub(b.openedAt)
		return nil, &OpenError{RetryAfter: remaining}
	case HalfOpen:
		if b.probes >= b.cfg.HalfOpenMaxRequests {
			return nil, &OpenError{RetryAfter: time.Second}
		}
		b.probes++
		gen := b.gen
		return func(success bool) { b.recordProbe(gen, success) }, nil
	default:
		gen := b.gen
		return func(success bool) { b.record(gen, success) }, nil
	}
}

func (b *CircuitBreaker) refreshLocked() {
	if b.state == Open && b.now().Sub(b.openedAt) >= b.cfg.OpenDuration {
		b.transitionLocked(HalfOpen, "open duration elapsed")
	}
}

// record folds an outcome into the ring buffer; outcomes from a previous
// state generation (e.g. requests still in flight when the breaker opened)
// are dropped so they cannot corrupt the new state's stats.
func (b *CircuitBreaker) record(gen uint64, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen != b.gen || b.state != Closed {
		return
	}

	if b.count == len(b.outcomes) {
		if b.outcomes[b.head] {
			b.failures--
		}
	} else {
		b.count++
	}
	failure := !success
	b.outcomes[b.head] = failure
	if failure {
		b.failures++
	}
	b.head = (b.head + 1) % len(b.outcomes)

	if failure && b.count == len(b.outcomes) {
		ratio := float64(b.failures) / float64(b.count)
		if ratio >= b.cfg.FailureThreshold {
			b.openLocked(fmt.Sprintf("failure ratio %.2f over last %d requests", ratio, b.count))
		}
	}
}

func (b *CircuitBreaker) recordProbe(gen uint64, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen != b.gen || b.state != HalfOpen {
		return
	}
	if success {
		b.transitionLocked(Closed, "half-open probe succeeded")
	} else {
		b.openLocked("half-open probe failed")
	}
}

func (b *CircuitBreaker) openLocked(reason string) {
	b.openedAt = b.now()
	b.transitionLocked(Open, reason)
}

func (b *CircuitBreaker) transitionLocked(to State, reason string) {
	from := b.state
	b.state = to
	b.gen++
	b.probes = 0
	b.count = 0
	b.failures = 0
	b.head = 0
	b.logger.Info("circuit breaker state change",
		"upstream", b.name,
		"from", from.String(),
		"to", to.String(),
		"reason", reason,
	)
	if b.onStateChange != nil {
		b.onStateChange(b.name, from, to)
	}
}
