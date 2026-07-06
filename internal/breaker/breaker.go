// Package breaker will contain the per-upstream circuit breaker.
// For now it only defines the interface the proxy middleware depends on.
package breaker

import (
	"errors"
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
// request: it returns ErrOpen (or an OpenError with the remaining open time)
// when the request must be short-circuited, otherwise a done callback that
// must be called with the outcome so half-open probe slots are released.
type Breaker interface {
	Allow() (done func(success bool), err error)
	State() State
}

type OpenError struct {
	RetryAfter time.Duration
}

func (e *OpenError) Error() string { return ErrOpen.Error() }

func (e *OpenError) Unwrap() error { return ErrOpen }
