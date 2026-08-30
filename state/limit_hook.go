package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type limitHookEvent struct {
	UID                        int       `json:"uid"`
	Username                   string    `json:"username"`
	EnforceableCPUUsagePercent float64   `json:"enforceable_cpu_usage_percent"`
	CPUEligibleUsersCount      int       `json:"cpu_eligible_users_count"`
	SharedCgroup               string    `json:"shared_cgroup"`
	Timestamp                  time.Time `json:"timestamp"`
	ServerRole                 string    `json:"server_role,omitempty"`
	LimitHookSource            string    `json:"source"`
}

type sanitizedHookError struct {
	message string
	cause   error
}

type hookOutcomeLogger interface {
	Info(msg string, keyvals ...interface{})
	Warn(msg string, keyvals ...interface{})
}

func (e *sanitizedHookError) Error() string {
	return e.message
}

func (e *sanitizedHookError) Unwrap() error {
	return e.cause
}

func (m *Manager) notifyUserLimited(cfg *config.Config, uid int, username string, metrics *SystemMetrics) {
	if cfg == nil || !cfg.LimitHookEnabled {
		return
	}

	m.mu.RLock()
	sharedCgroup := m.sharedCgroupPath
	m.mu.RUnlock()

	event := limitHookEvent{
		UID:                        uid,
		Username:                   username,
		EnforceableCPUUsagePercent: userEnforceableCPUUsage(metrics, uid),
		CPUEligibleUsersCount:      metrics.CPUEligibleUsersCount,
		SharedCgroup:               sharedCgroup,
		Timestamp:                  time.Now().UTC(),
		ServerRole:                 cfg.ServerRole,
		LimitHookSource:            "resman",
	}

	m.hookMu.Lock()
	if m.hookClosed {
		m.hookMu.Unlock()
		if cfg.LimitHookScript != "" {
			m.recordLimitHookResult(event, resmanmetrics.LimitHookTypeScript, "", context.Canceled)
		}
		if cfg.LimitHookURL != "" {
			m.recordLimitHookResult(event, resmanmetrics.LimitHookTypeHTTP, cfg.LimitHookURL, context.Canceled)
		}
		return
	}
	parentCtx := m.hookCtx
	m.hookWG.Add(1)
	m.hookMu.Unlock()

	go func() {
		defer m.hookWG.Done()
		m.runLimitHook(parentCtx, cfg, event)
	}()
}

func (m *Manager) runLimitHook(parentCtx context.Context, cfg *config.Config, event limitHookEvent) {
	timeout := time.Duration(cfg.LimitHookTimeout) * time.Second
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if cfg.LimitHookScript != "" {
		err := m.executeHookScript(ctx, cfg.LimitHookScript, event)
		m.recordLimitHookResult(event, resmanmetrics.LimitHookTypeScript, "", err)
	}

	if cfg.LimitHookURL != "" {
		err := m.executeHookRequest(ctx, cfg.LimitHookURL, event)
		m.recordLimitHookResult(event, resmanmetrics.LimitHookTypeHTTP, cfg.LimitHookURL, err)
	}
}

func (m *Manager) stopLimitHooks() {
	m.hookMu.Lock()
	if m.hookClosed {
		m.hookMu.Unlock()
		m.hookWG.Wait()
		return
	}
	m.hookClosed = true
	cancel := m.hookCancel
	m.hookMu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.hookWG.Wait()
}

func (m *Manager) recordLimitHookResult(event limitHookEvent, hookType resmanmetrics.LimitHookType, endpoint string, err error) {
	outcome := limitHookOutcome(err)
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordLimitHookExecution(hookType, outcome)
	}
	if err == nil {
		m.logger.Info("Limit hook execution completed",
			"uid", event.UID,
			"username", event.Username,
			"hook_type", hookType,
			"outcome", outcome,
		)
		return
	}
	if hookType == resmanmetrics.LimitHookTypeHTTP {
		reportLimitHookURLFailure(m.logger, event, endpoint, outcome, err)
		return
	}
	reportLimitHookScriptFailure(m.logger, event, outcome, err)
}

func limitHookOutcome(err error) resmanmetrics.LimitHookOutcome {
	switch {
	case err == nil:
		return resmanmetrics.LimitHookOutcomeSuccess
	case errors.Is(err, context.DeadlineExceeded):
		return resmanmetrics.LimitHookOutcomeTimeout
	case errors.Is(err, context.Canceled):
		return resmanmetrics.LimitHookOutcomeCancelled
	default:
		return resmanmetrics.LimitHookOutcomeFailure
	}
}

func reportLimitHookScriptFailure(logger hookOutcomeLogger, event limitHookEvent, outcome resmanmetrics.LimitHookOutcome, err error) {
	logger.Warn("Limit hook script failed",
		"uid", event.UID,
		"username", event.Username,
		"outcome", outcome,
		"error", err,
	)
}

func reportLimitHookURLFailure(logger hookOutcomeLogger, event limitHookEvent, endpoint string, outcome resmanmetrics.LimitHookOutcome, err error) {
	logger.Warn("Limit hook webservice failed",
		"uid", event.UID,
		"username", event.Username,
		"endpoint", hookEndpointForLog(endpoint),
		"outcome", outcome,
		"error", err,
	)
}

func runLimitHookScript(ctx context.Context, script string, event limitHookEvent) error {
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(),
		"RESMAN_LIMIT_UID="+strconv.Itoa(event.UID),
		"RESMAN_LIMIT_USERNAME="+event.Username,
		"RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT="+strconv.FormatFloat(event.EnforceableCPUUsagePercent, 'f', 2, 64),
		"RESMAN_LIMIT_CPU_ELIGIBLE_USERS_COUNT="+strconv.Itoa(event.CPUEligibleUsersCount),
		"RESMAN_LIMIT_SHARED_CGROUP="+event.SharedCgroup,
		"RESMAN_LIMIT_TIMESTAMP="+event.Timestamp.Format(time.RFC3339),
		"RESMAN_LIMIT_SERVER_ROLE="+event.ServerRole,
	)

	if err := cmd.Run(); err != nil {
		return sanitizeScriptHookError(ctx, err)
	}
	return nil
}

func postLimitHook(ctx context.Context, endpoint string, event limitHookEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal hook event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return newSanitizedHookError("create hook request", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "resman-limit-hook")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return newSanitizedHookError("post hook request", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hook endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func sanitizeScriptHookError(ctx context.Context, err error) error {
	reason := "execution failed"
	cause := err
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = "timed out"
		cause = context.DeadlineExceeded
	} else if errors.Is(ctx.Err(), context.Canceled) {
		reason = "canceled"
		cause = context.Canceled
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			reason = fmt.Sprintf("exited with status %d", exitErr.ExitCode())
		}
	}
	return &sanitizedHookError{
		message: "limit hook script " + reason,
		cause:   cause,
	}
}

func newSanitizedHookError(operation, endpoint string, err error) error {
	reason := "failed"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timed out"
	} else if errors.Is(err, context.Canceled) {
		reason = "canceled"
	}
	return &sanitizedHookError{
		message: fmt.Sprintf("%s for %s %s", operation, hookEndpointForLog(endpoint), reason),
		cause:   safeHookCause(err),
	}
}

func safeHookCause(err error) error {
	// HTTP transports and URL parsers may include the complete request URL in
	// their error chain. Preserve only bounded context sentinels whose text
	// cannot contain credentials, paths, query values, or fragments.
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func hookEndpointForLog(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<invalid-hook-endpoint>"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}
