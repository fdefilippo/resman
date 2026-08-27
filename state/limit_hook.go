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
)

type limitHookEvent struct {
	UID             int       `json:"uid"`
	Username        string    `json:"username"`
	CPUUsage        float64   `json:"cpu_usage"`
	LimitedUsers    int       `json:"limited_users"`
	SharedCgroup    string    `json:"shared_cgroup"`
	Timestamp       time.Time `json:"timestamp"`
	ServerRole      string    `json:"server_role,omitempty"`
	LimitHookSource string    `json:"source"`
}

type sanitizedHookError struct {
	message string
	cause   error
}

type hookWarningLogger interface {
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
		UID:             uid,
		Username:        username,
		CPUUsage:        userEnforceableCPUUsage(metrics, uid),
		LimitedUsers:    metrics.CPUEligibleUsersCount,
		SharedCgroup:    sharedCgroup,
		Timestamp:       time.Now().UTC(),
		ServerRole:      cfg.ServerRole,
		LimitHookSource: "resman",
	}

	go m.runLimitHook(cfg, event)
}

func (m *Manager) runLimitHook(cfg *config.Config, event limitHookEvent) {
	timeout := time.Duration(cfg.LimitHookTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if cfg.LimitHookScript != "" {
		if err := runLimitHookScript(ctx, cfg.LimitHookScript, event); err != nil {
			reportLimitHookScriptFailure(m.logger, event, err)
		}
	}

	if cfg.LimitHookURL != "" {
		if err := postLimitHook(ctx, cfg.LimitHookURL, event); err != nil {
			reportLimitHookURLFailure(m.logger, event, cfg.LimitHookURL, err)
		}
	}
}

func reportLimitHookScriptFailure(logger hookWarningLogger, event limitHookEvent, err error) {
	logger.Warn("Limit hook script failed",
		"uid", event.UID,
		"username", event.Username,
		"error", err,
	)
}

func reportLimitHookURLFailure(logger hookWarningLogger, event limitHookEvent, endpoint string, err error) {
	logger.Warn("Limit hook webservice failed",
		"uid", event.UID,
		"username", event.Username,
		"endpoint", hookEndpointForLog(endpoint),
		"error", err,
	)
}

func runLimitHookScript(ctx context.Context, script string, event limitHookEvent) error {
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(),
		"RESMAN_LIMIT_UID="+strconv.Itoa(event.UID),
		"RESMAN_LIMIT_USERNAME="+event.Username,
		"RESMAN_LIMIT_CPU_USAGE="+strconv.FormatFloat(event.CPUUsage, 'f', 2, 64),
		"RESMAN_LIMIT_LIMITED_USERS="+strconv.Itoa(event.LimitedUsers),
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
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = "timed out"
	} else if errors.Is(ctx.Err(), context.Canceled) {
		reason = "canceled"
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			reason = fmt.Sprintf("exited with status %d", exitErr.ExitCode())
		}
	}
	return &sanitizedHookError{
		message: "limit hook script " + reason,
		cause:   err,
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
