package proxy

import (
	"context"
	"errors"
	"time"
)

const (
	defaultUpstreamEmptyRetryCount   = 4
	defaultUpstreamEmptyRetryBackoff = 500 * time.Millisecond
	maxUpstreamEmptyRetryDelay       = 30 * time.Second
)

type upstreamEmptyRetryPolicy struct {
	count   int
	backoff time.Duration
}

func newDefaultUpstreamEmptyRetryPolicy() upstreamEmptyRetryPolicy {
	return upstreamEmptyRetryPolicy{
		count:   defaultUpstreamEmptyRetryCount,
		backoff: defaultUpstreamEmptyRetryBackoff,
	}
}

func newUpstreamEmptyRetryPolicy(count int, backoff time.Duration) upstreamEmptyRetryPolicy {
	if count < 0 {
		count = 0
	}
	if backoff < 0 {
		backoff = 0
	}
	return upstreamEmptyRetryPolicy{
		count:   count,
		backoff: backoff,
	}
}

func (p upstreamEmptyRetryPolicy) shouldRetry(attempt int, doneReceived, contentEmitted bool, resultLen int, scanErr error, clientGone bool) bool {
	if attempt >= p.count {
		return false
	}
	if clientGone || errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
		return false
	}
	return !doneReceived && !contentEmitted && resultLen == 0
}

func (p upstreamEmptyRetryPolicy) delay(attempt int) time.Duration {
	if p.backoff <= 0 {
		return 0
	}
	delay := p.backoff
	for i := 0; i < attempt; i++ {
		if delay >= maxUpstreamEmptyRetryDelay/2 {
			return maxUpstreamEmptyRetryDelay
		}
		delay *= 2
	}
	if delay > maxUpstreamEmptyRetryDelay {
		return maxUpstreamEmptyRetryDelay
	}
	return delay
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
