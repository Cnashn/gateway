package breaker_test

import (
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/cnashn/gateway/internal/breaker"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newBreaker(t *testing.T, cfg breaker.Config) (*breaker.CircuitBreaker, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return breaker.New("orders", cfg, breaker.WithClock(clock.Now), breaker.WithLogger(logger)), clock
}

func mustAllow(t *testing.T, b *breaker.CircuitBreaker) func(bool) {
	t.Helper()
	done, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() error = %v, want allowed", err)
	}
	return done
}

var testConfig = breaker.Config{
	FailureThreshold:    0.5,
	WindowSize:          4,
	OpenDuration:        30 * time.Second,
	HalfOpenMaxRequests: 2,
}

func TestClosedToOpenOnFailureRatio(t *testing.T) {
	b, _ := newBreaker(t, testConfig)

	mustAllow(t, b)(true)
	mustAllow(t, b)(true)
	mustAllow(t, b)(false)
	if b.State() != breaker.Closed {
		t.Fatalf("state = %v after 3 of 4 window slots, want closed (window not full)", b.State())
	}

	mustAllow(t, b)(false)
	if b.State() != breaker.Open {
		t.Fatalf("state = %v after ratio 0.5 >= threshold 0.5, want open", b.State())
	}

	_, err := b.Allow()
	var oe *breaker.OpenError
	if !errors.As(err, &oe) {
		t.Fatalf("Allow() error = %v, want OpenError short-circuit", err)
	}
	if !errors.Is(err, breaker.ErrOpen) {
		t.Error("OpenError does not unwrap to ErrOpen")
	}
	if oe.RetryAfter <= 0 || oe.RetryAfter > 30*time.Second {
		t.Errorf("RetryAfter = %v, want in (0, 30s]", oe.RetryAfter)
	}
}

func TestBelowThresholdStaysClosed(t *testing.T) {
	b, _ := newBreaker(t, testConfig)

	for i := 0; i < 20; i++ {
		mustAllow(t, b)(i%4 != 0)
	}
	if b.State() != breaker.Closed {
		t.Errorf("state = %v with 25%% failures under 50%% threshold, want closed", b.State())
	}
}

func tripBreaker(t *testing.T, b *breaker.CircuitBreaker) {
	t.Helper()
	for i := 0; i < testConfig.WindowSize; i++ {
		mustAllow(t, b)(false)
	}
	if b.State() != breaker.Open {
		t.Fatalf("state = %v after all-failure window, want open", b.State())
	}
}

func TestOpenToHalfOpenAfterDuration(t *testing.T) {
	b, clock := newBreaker(t, testConfig)
	tripBreaker(t, b)

	clock.Advance(29 * time.Second)
	if _, err := b.Allow(); err == nil {
		t.Fatal("Allow() succeeded 1s before open duration elapsed, want short-circuit")
	}

	clock.Advance(2 * time.Second)
	if b.State() != breaker.HalfOpen {
		t.Fatalf("state = %v after open duration elapsed, want half-open", b.State())
	}
	mustAllow(t, b)
}

func TestHalfOpenProbeSuccessCloses(t *testing.T) {
	b, clock := newBreaker(t, testConfig)
	tripBreaker(t, b)
	clock.Advance(31 * time.Second)

	mustAllow(t, b)(true)
	if b.State() != breaker.Closed {
		t.Errorf("state = %v after successful probe, want closed", b.State())
	}
	mustAllow(t, b)
}

func TestHalfOpenProbeFailureReopensAndResetsTimer(t *testing.T) {
	b, clock := newBreaker(t, testConfig)
	tripBreaker(t, b)
	clock.Advance(31 * time.Second)

	mustAllow(t, b)(false)
	if b.State() != breaker.Open {
		t.Fatalf("state = %v after failed probe, want open", b.State())
	}

	_, err := b.Allow()
	var oe *breaker.OpenError
	if !errors.As(err, &oe) {
		t.Fatalf("Allow() error = %v, want OpenError", err)
	}
	if oe.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want full 30s: a failed probe must reset the open timer", oe.RetryAfter)
	}
}

func TestHalfOpenLimitsConcurrentProbes(t *testing.T) {
	b, clock := newBreaker(t, testConfig)
	tripBreaker(t, b)
	clock.Advance(31 * time.Second)

	done1 := mustAllow(t, b)
	mustAllow(t, b)

	if _, err := b.Allow(); err == nil {
		t.Fatal("third concurrent probe allowed, want rejected (half_open_max_requests=2)")
	}

	done1(true)
	mustAllow(t, b)
}

func TestStaleOutcomesFromPreviousStateIgnored(t *testing.T) {
	b, _ := newBreaker(t, testConfig)

	inFlight := mustAllow(t, b)
	tripBreaker(t, b)

	inFlight(true)
	if b.State() != breaker.Open {
		t.Errorf("state = %v, want open: outcome from before the transition must be dropped", b.State())
	}
}

func TestStateChangeHook(t *testing.T) {
	var transitions []string
	clock := newFakeClock()
	b := breaker.New("orders", testConfig,
		breaker.WithClock(clock.Now),
		breaker.WithLogger(slog.New(slog.NewJSONHandler(io.Discard, nil))),
		breaker.WithOnStateChange(func(upstream string, from, to breaker.State) {
			transitions = append(transitions, upstream+":"+from.String()+"->"+to.String())
		}),
	)

	for i := 0; i < testConfig.WindowSize; i++ {
		done, err := b.Allow()
		if err != nil {
			t.Fatal(err)
		}
		done(false)
	}
	clock.Advance(31 * time.Second)
	done, err := b.Allow()
	if err != nil {
		t.Fatal(err)
	}
	done(true)

	want := []string{"orders:closed->open", "orders:open->half-open", "orders:half-open->closed"}
	if len(transitions) != 3 || transitions[0] != want[0] || transitions[1] != want[1] || transitions[2] != want[2] {
		t.Errorf("transitions = %v, want %v", transitions, want)
	}
}

func TestConcurrentUseDuringTransitions(t *testing.T) {
	b, clock := newBreaker(t, breaker.Config{
		FailureThreshold:    0.5,
		WindowSize:          8,
		OpenDuration:        10 * time.Millisecond,
		HalfOpenMaxRequests: 2,
	})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 500; i++ {
				if done, err := b.Allow(); err == nil {
					done(rng.Intn(3) != 0)
				}
				if i%50 == 0 {
					clock.Advance(5 * time.Millisecond)
				}
			}
		}(int64(g))
	}
	wg.Wait()

	s := b.State()
	if s != breaker.Closed && s != breaker.Open && s != breaker.HalfOpen {
		t.Errorf("state = %v, want a valid state", s)
	}
}
