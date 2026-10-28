package sdk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"time"
)

// RetryPolicy decides which failed Router calls (client.Models) are retried
// and for how long. It does not govern the workflow surface (Submit, Run),
// whose 429 handling follows the server's Retry-After.
//
// Retried without asking: a 429 that names Retry-After, and a failure that
// never reached the server (refused connection, connect timeout). A 5xx or a
// connection dropped mid-request may have run, and a model run is billed, so
// those retry only with RetryPossiblyInFlight. A run the Router is still
// working on (409 concurrency_limit_exceeded or 504 deadline_exceeded, each
// with Retry-After) is collected under the same key on its own longer budget.
type RetryPolicy struct {
	// MaxElapsed bounds the whole retry window; zero disables retrying.
	MaxElapsed time.Duration
	// InitialBackoff, BackoffFactor, and MaxBackoff shape the exponential
	// backoff used when the server names no Retry-After.
	InitialBackoff time.Duration
	BackoffFactor  float64
	MaxBackoff     time.Duration
	// Jitter draws each wait uniformly below its ceiling.
	Jitter bool
	// RetryPossiblyInFlight also retries a 5xx or a dropped connection.
	RetryPossiblyInFlight bool
	// RetryCollectable collects a still-running generation under its key.
	RetryCollectable bool
	// CollectMaxElapsed bounds collecting; generations can take minutes.
	CollectMaxElapsed time.Duration
}

// DefaultRetry is the policy client.Models uses unless replaced.
var DefaultRetry = RetryPolicy{
	MaxElapsed:        60 * time.Second,
	InitialBackoff:    500 * time.Millisecond,
	BackoffFactor:     2,
	MaxBackoff:        15 * time.Second,
	Jitter:            true,
	RetryCollectable:  true,
	CollectMaxElapsed: 20 * time.Minute,
}

// NoRetry makes every Router call exactly one attempt.
var NoRetry = RetryPolicy{InitialBackoff: 500 * time.Millisecond, BackoffFactor: 2, MaxBackoff: 15 * time.Second}

// Validate reports a policy that cannot be followed.
func (p RetryPolicy) Validate() error {
	switch {
	case p.MaxElapsed < 0:
		return errors.New("MaxElapsed must not be negative")
	case p.CollectMaxElapsed < 0:
		return errors.New("CollectMaxElapsed must not be negative")
	case p.InitialBackoff <= 0:
		return errors.New("InitialBackoff must be positive")
	case p.BackoffFactor < 1 || math.IsNaN(p.BackoffFactor) || math.IsInf(p.BackoffFactor, 0):
		return errors.New("BackoffFactor must be a finite number >= 1")
	case p.MaxBackoff < p.InitialBackoff:
		return errors.New("MaxBackoff must be >= InitialBackoff")
	}
	return nil
}

func (p RetryPolicy) enabled() bool { return p.MaxElapsed > 0 }

// retryAfterOf is the error's Retry-After, nil when absent or negative.
func retryAfterOf(err error) *time.Duration {
	var api *APIError
	if !errors.As(err, &api) || api.RetryAfter == nil || *api.RetryAfter < 0 {
		return nil
	}
	d := time.Duration(min(*api.RetryAfter, maxRetryAfter)) * retryAfterUnit
	return &d
}

func bucketOf(api *APIError) string {
	if api.ErrorType != "" {
		return api.ErrorType
	}
	return api.Code
}

// isCollectable is a generation still running: a 409 concurrency limit or a
// 504 deadline, either with Retry-After.
func isCollectable(err error) bool {
	var api *APIError
	if !errors.As(err, &api) || retryAfterOf(err) == nil {
		return false
	}
	switch api.HTTPStatus {
	case 409:
		return bucketOf(api) == "concurrency_limit_exceeded"
	case 504:
		return bucketOf(api) == "deadline_exceeded"
	}
	return false
}

// neverDelivered is a failure before the request left: DNS, refused
// connection, connect timeout. possiblyInFlight is anything after.
func neverDelivered(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Op == "proxyconnect" {
		return true
	}
	return false
}

func possiblyInFlight(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && !errors.Is(err, context.Canceled)
}

// shouldRetry ports RetryPolicy.should_retry.
func (p RetryPolicy) shouldRetry(err error) bool {
	var api *APIError
	if errors.As(err, &api) && api.HTTPStatus != 0 {
		switch {
		case api.HTTPStatus == 429:
			return retryAfterOf(err) != nil
		case isCollectable(err):
			return p.RetryCollectable
		case api.HTTPStatus >= 500 && api.HTTPStatus <= 599:
			return p.RetryPossiblyInFlight
		}
		return false
	}
	if neverDelivered(err) {
		return true
	}
	if possiblyInFlight(err) {
		return p.RetryPossiblyInFlight
	}
	return false
}

// mayHaveClaimedKey reports whether a failed attempt might have reached the
// Router and claimed its Idempotency-Key, so a later reuse rejection means
// the generation may already exist.
func mayHaveClaimedKey(err error) bool {
	var api *APIError
	if errors.As(err, &api) && api.HTTPStatus != 0 {
		if api.HTTPStatus == 429 {
			return false
		}
		if isCollectable(err) {
			return true
		}
		return api.HTTPStatus >= 500 && api.HTTPStatus <= 599
	}
	if neverDelivered(err) {
		return false
	}
	return possiblyInFlight(err)
}

func (p RetryPolicy) ceiling(exponent int) time.Duration {
	if p.BackoffFactor == 1 || p.InitialBackoff >= p.MaxBackoff {
		return min(p.InitialBackoff, p.MaxBackoff)
	}
	reachesAt := math.Log(float64(p.MaxBackoff)/float64(p.InitialBackoff)) / math.Log(p.BackoffFactor)
	if float64(exponent) >= reachesAt {
		return p.MaxBackoff
	}
	return min(p.MaxBackoff, time.Duration(float64(p.InitialBackoff)*math.Pow(p.BackoffFactor, float64(exponent))))
}

func (p RetryPolicy) backoff(attempt int) time.Duration {
	c := p.ceiling(max(attempt-1, 0))
	if p.Jitter {
		return time.Duration(rand.Float64() * float64(c))
	}
	return c
}

// retrier tracks one call's attempts against its policy.
type retrier struct {
	policy   RetryPolicy
	started  time.Time
	attempts int
}

func newRetrier(p RetryPolicy) *retrier { return &retrier{policy: p, started: time.Now()} }

// delay is how long to wait before retrying after err, or false to surface it.
func (r *retrier) delay(err error) (time.Duration, bool) {
	failedAt := time.Now()
	r.attempts++
	if !r.policy.enabled() || !r.policy.shouldRetry(err) {
		return 0, false
	}
	budget := r.policy.MaxElapsed
	if r.policy.RetryCollectable && isCollectable(err) {
		budget = r.policy.CollectMaxElapsed
	}
	remaining := r.started.Add(budget).Sub(failedAt)
	if remaining <= 0 {
		return 0, false
	}
	d := r.policy.backoff(r.attempts)
	if named := retryAfterOf(err); named != nil && *named > 0 {
		d = *named
	}
	return min(d, remaining), true
}

// withRetry runs send until it succeeds or the policy says to surface the
// error.
func withRetry[T any](ctx context.Context, p RetryPolicy, send func() (T, error)) (T, error) {
	r := newRetrier(p)
	for {
		v, err := send()
		if err == nil {
			return v, nil
		}
		d, ok := r.delay(err)
		if !ok {
			return v, err
		}
		if serr := sleep(ctx, d); serr != nil {
			return v, fmt.Errorf("%w (while waiting to retry after: %v)", serr, err)
		}
	}
}
