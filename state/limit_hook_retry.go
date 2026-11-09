package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

// hookHTTPStatusError retains only a bounded status, never endpoint/body secrets.
type hookHTTPStatusError struct{ status int }

func (e *hookHTTPStatusError) Error() string {
	return fmt.Sprintf("hook endpoint returned HTTP %d", e.status)
}
func (e *hookHTTPStatusError) Retryable() bool {
	switch e.status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return false
}
func (e *sanitizedHookError) Retryable() bool { return e.retryable }
func limitHookRetryable(err error) bool {
	var classified interface{ Retryable() bool }
	return errors.As(err, &classified) && classified.Retryable()
}

// transientHookTransportError must run before sanitization strips URL-bearing errors.
func transientHookTransportError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	for _, transient := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && (dns.IsTemporary || dns.IsTimeout) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) && network.Timeout()
}

func waitLimitHookRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(1<<(attempt-1)) * 100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attemptReporter is optional for test exporters; the production exporter implements it.
type attemptReporter interface {
	RecordLimitHookAttempt(resmanmetrics.LimitHookType, resmanmetrics.LimitHookOutcome)
}

var _ attemptReporter = (*resmanmetrics.PrometheusExporter)(nil)

func (m *Manager) recordLimitHookAttempt(job limitHookJob, err error) {
	if reporter, ok := m.prometheusExporter.(attemptReporter); ok {
		reporter.RecordLimitHookAttempt(job.hookType, limitHookOutcome(err))
	}
	if err != nil {
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError("limit_hook", "attempt_failure")
		}
		m.logger.Warn("Limit hook attempt failed", "hook_type", job.hookType, "event_id", job.event.EventID, "outcome", limitHookOutcome(err))
	}
}

// hookSingleAttemptTransport leaves redirect replay with Client, while an actual
// transport request cannot replay a body behind the executor's attempt counter.
type hookSingleAttemptTransport struct{ next http.RoundTripper }

func (t hookSingleAttemptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	attempt := request.Clone(request.Context())
	attempt.GetBody = nil
	return t.next.RoundTrip(attempt)
}
