package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/limithook"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type limitHookEvent struct {
	UID                               int                                    `json:"uid"`
	Username                          string                                 `json:"username"`
	EnforceableCPUUsagePercent        float64                                `json:"enforceable_cpu_usage_percent"`
	CPUEligibleUsersCount             int                                    `json:"cpu_eligible_users_count"`
	SharedCgroup                      string                                 `json:"shared_cgroup"`
	Timestamp                         time.Time                              `json:"timestamp"`
	ServerRole                        string                                 `json:"server_role,omitempty"`
	LimitHookSource                   string                                 `json:"source"`
	CPUPointsConfiguredClass          string                                 `json:"cpu_points_configured_class"`
	CPUPointsConfiguredGuarantee      *uint64                                `json:"cpu_points_configured_guarantee,omitempty"`
	CPUPointsLifecycleState           resmanmetrics.CPUPointsLifecycleState  `json:"cpu_points_lifecycle_state"`
	CPUPointsAppliedClass             *string                                `json:"cpu_points_applied_class,omitempty"`
	CPUPointsAppliedWeight            *uint64                                `json:"cpu_points_applied_weight,omitempty"`
	CPUPointsAppliedToProcesses       bool                                   `json:"cpu_points_applied_to_processes"`
	CPUPointsCompleteUIDGuaranteed    bool                                   `json:"cpu_points_complete_uid_workload_guaranteed"`
	CPUPointsReconciliationDegraded   bool                                   `json:"cpu_points_reconciliation_degraded"`
	CPUPointsProcessCoverage          resmanmetrics.CPUPointsProcessCoverage `json:"cpu_points_process_coverage"`
	PIDNamespaceMismatchCount         int                                    `json:"pid_namespace_mismatch_count"`
	PIDNamespaceUnavailableCount      int                                    `json:"pid_namespace_unavailable_count"`
	RAMCgroupMemoryCurrentBytes       *uint64                                `json:"ram_cgroup_memory_current_bytes,omitempty"`
	RAMCoverage                       *string                                `json:"ram_coverage,omitempty"`
	RAMCoverageIncompleteProcessCount int                                    `json:"ram_coverage_incomplete_process_count"`
	RAMSwapDisabled                   *bool                                  `json:"ram_swap_disabled,omitempty"`
	MemoryHighLimit                   *string                                `json:"memory_high_limit,omitempty"`
	MemoryMaxLimit                    *string                                `json:"memory_max_limit,omitempty"`
	MemorySwapMax                     *string                                `json:"memory_swap_max,omitempty"`
	MemoryHighEventsDelta             *uint64                                `json:"memory_high_events_delta,omitempty"`
	MemoryMaxEventsDelta              *uint64                                `json:"memory_max_events_delta,omitempty"`
	MemoryOOMEventsDelta              *uint64                                `json:"memory_oom_events_delta,omitempty"`
	MemoryOOMKillEventsDelta          *uint64                                `json:"memory_oom_kill_events_delta,omitempty"`
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

	cpuPoints := m.limitHookCPUPointsSnapshot(uid, username, metrics)
	m.mu.RLock()
	sharedCgroup := m.sharedCgroupPath
	m.mu.RUnlock()

	event := limitHookEvent{
		UID:                               uid,
		Username:                          username,
		EnforceableCPUUsagePercent:        userEnforceableCPUUsage(metrics, uid),
		CPUEligibleUsersCount:             metrics.CPUEligibleUsersCount,
		SharedCgroup:                      sharedCgroup,
		Timestamp:                         time.Now().UTC(),
		ServerRole:                        cfg.ServerRole,
		LimitHookSource:                   "resman",
		CPUPointsConfiguredClass:          cpuPoints.ConfiguredClass,
		CPUPointsConfiguredGuarantee:      cpuPoints.ConfiguredGuaranteePoints,
		CPUPointsLifecycleState:           cpuPoints.LifecycleState,
		CPUPointsAppliedClass:             cpuPoints.AppliedClass,
		CPUPointsAppliedWeight:            cpuPoints.AppliedWeight,
		CPUPointsAppliedToProcesses:       cpuPoints.AppliedToProcesses,
		CPUPointsCompleteUIDGuaranteed:    cpuPoints.CompleteUIDWorkloadGuaranteed,
		CPUPointsReconciliationDegraded:   cpuPoints.ReconciliationDegraded,
		CPUPointsProcessCoverage:          cpuPoints.ProcessCoverage,
		PIDNamespaceMismatchCount:         cpuPoints.PIDNamespaceMismatchCount,
		PIDNamespaceUnavailableCount:      cpuPoints.PIDNamespaceUnavailableCount,
		RAMCgroupMemoryCurrentBytes:       cpuPoints.RAMCgroupUsageBytes,
		RAMCoverage:                       cpuPoints.RAMCoverage,
		RAMCoverageIncompleteProcessCount: cpuPoints.RAMCoverageIncompleteProcessCount,
		RAMSwapDisabled:                   cpuPoints.RAMSwapDisabled,
		MemoryHighLimit:                   cpuPoints.MemoryHighLimit,
		MemoryMaxLimit:                    cpuPoints.MemoryMaxLimit,
		MemorySwapMax:                     cpuPoints.MemorySwapMax,
		MemoryHighEventsDelta:             cpuPoints.MemoryHighEventsDelta,
		MemoryMaxEventsDelta:              cpuPoints.MemoryMaxEventsDelta,
		MemoryOOMEventsDelta:              cpuPoints.MemoryOOMEventsDelta,
		MemoryOOMKillEventsDelta:          cpuPoints.MemoryOOMKillEventsDelta,
	}

	m.dispatchLimitHookJobs(cfg, event)
}

func (m *Manager) limitHookCPUPointsSnapshot(uid int, username string, sample *SystemMetrics) resmanmetrics.CPUPointsUserSnapshot {
	result := resmanmetrics.CPUPointsUserSnapshot{UID: uid, Username: username, ProcessCoverage: resmanmetrics.CPUPointsCoverageNone}
	if sample != nil {
		if observed, ok := sample.CPUPointsUsers[uid]; ok {
			result = observed
		}
	}
	m.mu.RLock()
	policy := m.cpuPointsPolicy
	allocation, applied := m.cpuAllocations[uid]
	event := m.cpuPointsLifecycleEvents[uid]
	resource := m.resourceLimits[uid]
	ramCoverage := m.ramCoverage[uid]
	result.ReconciliationDegraded = m.cpuPointsDegraded
	m.mu.RUnlock()
	result.UID, result.Username = uid, username
	result.ConfiguredClass = string(policy.ClassForUID(uid))
	if guarantee, ok := policy.GuaranteeForUID(uid); ok {
		points := guarantee.Points().Value()
		result.ConfiguredGuaranteePoints = &points
	} else {
		result.ConfiguredGuaranteePoints = nil
	}
	if sample != nil && sample.UserMetrics[uid] != nil {
		observed := sample.UserMetrics[uid]
		result.CPUEnforcementRequested = observed.CPULimitRequested
		result.ObservedProcessCount = observed.ProcessCount
		result.EnforceableProcessCount = observed.EnforceableUsage.ProcessCount
	}
	result.PIDNamespaceMismatchCount = event.pidNamespaceMismatches
	result.PIDNamespaceUnavailableCount = event.pidNamespaceUnavailable
	acquired := result.EnforceableProcessCount - event.pidNamespaceMismatches - event.pidNamespaceUnavailable
	if applied && acquired > 0 {
		class := string(allocation.class)
		weight := uint64(allocation.weight.Value())
		result.AppliedClass, result.AppliedWeight = &class, &weight
		result.AppliedToProcesses = true
		result.LifecycleState = resmanmetrics.CPUPointsLifecycleApplied
		result.ProcessCoverage = resmanmetrics.CPUPointsCoverageComplete
		if acquired != result.ObservedProcessCount || result.ObservedProcessCount != result.EnforceableProcessCount || event.pidNamespaceMismatches > 0 || event.pidNamespaceUnavailable > 0 {
			result.ProcessCoverage = resmanmetrics.CPUPointsCoveragePartial
		}
		result.CompleteUIDWorkloadGuaranteed = result.ProcessCoverage == resmanmetrics.CPUPointsCoverageComplete
	}
	if resource.ramApplied {
		coverage := string(RAMCoverageComplete)
		if ramCoverage.coverage == RAMCoveragePartial {
			coverage = string(RAMCoveragePartial)
			result.RAMCoverageIncompleteProcessCount = len(ramCoverage.partial)
		}
		result.RAMCoverage = &coverage
		if memory, err := m.cgroupManager.GetMemoryAccountingSnapshot(uid); err == nil {
			current := memory.CurrentBytes
			result.RAMCgroupUsageBytes = &current
		}
	}
	return result
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
	if errors.Is(err, errLimitHookSaturated) {
		m.reportLimitHookSaturation(hookType)
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
	case errors.Is(err, errLimitHookSaturated):
		return resmanmetrics.LimitHookOutcomeSaturated
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

func runLimitHookScript(ctx context.Context, invocation hookScriptInvocation, event limitHookEvent) error {
	if err := limithook.ValidateScriptPath(invocation.path, invocation.identity); err != nil {
		return fmt.Errorf("validate limit hook script: %w", err)
	}

	cmd := exec.Command(invocation.path)
	cmd.Dir = "/"
	cmd.Env = append([]string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"LC_ALL=C",
		"HOME=/",
		"PWD=/",
		"USER=" + invocation.identity.Username,
		"LOGNAME=" + invocation.identity.Username,
	},
		"RESMAN_LIMIT_UID="+strconv.Itoa(event.UID),
		"RESMAN_LIMIT_USERNAME="+event.Username,
		"RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT="+strconv.FormatFloat(event.EnforceableCPUUsagePercent, 'f', 2, 64),
		"RESMAN_LIMIT_CPU_ELIGIBLE_USERS_COUNT="+strconv.Itoa(event.CPUEligibleUsersCount),
		"RESMAN_LIMIT_SHARED_CGROUP="+event.SharedCgroup,
		"RESMAN_LIMIT_TIMESTAMP="+event.Timestamp.Format(time.RFC3339),
		"RESMAN_LIMIT_SERVER_ROLE="+event.ServerRole,
		"RESMAN_LIMIT_CPU_POINTS_CONFIGURED_CLASS="+event.CPUPointsConfiguredClass,
		"RESMAN_LIMIT_CPU_POINTS_CONFIGURED_GUARANTEE="+formatOptionalUint64(event.CPUPointsConfiguredGuarantee),
		"RESMAN_LIMIT_CPU_POINTS_LIFECYCLE_STATE="+string(event.CPUPointsLifecycleState),
		"RESMAN_LIMIT_CPU_POINTS_APPLIED_CLASS="+formatOptionalString(event.CPUPointsAppliedClass),
		"RESMAN_LIMIT_CPU_POINTS_APPLIED_WEIGHT="+formatOptionalUint64(event.CPUPointsAppliedWeight),
		"RESMAN_LIMIT_CPU_POINTS_APPLIED_TO_PROCESSES="+strconv.FormatBool(event.CPUPointsAppliedToProcesses),
		"RESMAN_LIMIT_CPU_POINTS_COMPLETE_UID_WORKLOAD_GUARANTEED="+strconv.FormatBool(event.CPUPointsCompleteUIDGuaranteed),
		"RESMAN_LIMIT_CPU_POINTS_RECONCILIATION_DEGRADED="+strconv.FormatBool(event.CPUPointsReconciliationDegraded),
		"RESMAN_LIMIT_CPU_POINTS_PROCESS_COVERAGE="+string(event.CPUPointsProcessCoverage),
		"RESMAN_LIMIT_PID_NAMESPACE_MISMATCH_COUNT="+strconv.Itoa(event.PIDNamespaceMismatchCount),
		"RESMAN_LIMIT_PID_NAMESPACE_UNAVAILABLE_COUNT="+strconv.Itoa(event.PIDNamespaceUnavailableCount),
		"RESMAN_LIMIT_RAM_CGROUP_MEMORY_CURRENT_BYTES="+formatOptionalUint64(event.RAMCgroupMemoryCurrentBytes),
		"RESMAN_LIMIT_RAM_COVERAGE="+formatOptionalString(event.RAMCoverage),
		"RESMAN_LIMIT_RAM_COVERAGE_INCOMPLETE_PROCESS_COUNT="+strconv.Itoa(event.RAMCoverageIncompleteProcessCount),
		"RESMAN_LIMIT_RAM_SWAP_DISABLED="+formatOptionalBool(event.RAMSwapDisabled),
		"RESMAN_LIMIT_MEMORY_HIGH="+formatOptionalString(event.MemoryHighLimit),
		"RESMAN_LIMIT_MEMORY_MAX="+formatOptionalString(event.MemoryMaxLimit),
		"RESMAN_LIMIT_MEMORY_SWAP_MAX="+formatOptionalString(event.MemorySwapMax),
		"RESMAN_LIMIT_MEMORY_HIGH_EVENTS_DELTA="+formatOptionalUint64(event.MemoryHighEventsDelta),
		"RESMAN_LIMIT_MEMORY_MAX_EVENTS_DELTA="+formatOptionalUint64(event.MemoryMaxEventsDelta),
		"RESMAN_LIMIT_MEMORY_OOM_EVENTS_DELTA="+formatOptionalUint64(event.MemoryOOMEventsDelta),
		"RESMAN_LIMIT_MEMORY_OOM_KILL_EVENTS_DELTA="+formatOptionalUint64(event.MemoryOOMKillEventsDelta),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if invocation.dropPrivileges {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid:    invocation.identity.UID,
			Gid:    invocation.identity.GID,
			Groups: []uint32{},
		}
	}
	if err := cmd.Start(); err != nil {
		return sanitizeScriptHookError(ctx, err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		cleanupErr := cleanupCompletedLimitHookProcessGroup(cmd.Process.Pid)
		if err != nil {
			return sanitizeScriptHookError(ctx, err)
		}
		if cleanupErr != nil {
			return cleanupErr
		}
		return nil
	case <-ctx.Done():
		select {
		case err := <-waitDone:
			cleanupErr := cleanupCompletedLimitHookProcessGroup(cmd.Process.Pid)
			if err != nil {
				return sanitizeScriptHookError(ctx, err)
			}
			if cleanupErr != nil {
				return cleanupErr
			}
			return nil
		default:
		}
		terminationErr := terminateLimitHookProcessGroup(cmd.Process.Pid, waitDone)
		return sanitizeScriptHookError(ctx, terminationErr)
	}
}

func cleanupCompletedLimitHookProcessGroup(pgid int) error {
	err := syscall.Kill(-pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("inspect completed limit hook process group: %w", err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate background limit hook processes: %w", err)
	}
	if waitForLimitHookProcessGroupExitWithin(pgid, 500*time.Millisecond) == nil {
		return fmt.Errorf("limit hook script left background processes after exit")
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill background limit hook processes: %w", err)
	}
	if err := waitForLimitHookProcessGroupExit(pgid); err != nil {
		return err
	}
	return fmt.Errorf("limit hook script left background processes after exit")
}

func terminateLimitHookProcessGroup(pgid int, waitDone <-chan error) error {
	termErr := syscall.Kill(-pgid, syscall.SIGTERM)
	if termErr != nil && !errors.Is(termErr, syscall.ESRCH) {
		return fmt.Errorf("terminate limit hook process group: %w", termErr)
	}

	grace := time.NewTimer(500 * time.Millisecond)
	defer grace.Stop()
	var waitErr error
	waited := false
	select {
	case waitErr = <-waitDone:
		waited = true
	case <-grace.C:
	}
	if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return fmt.Errorf("kill limit hook process group: %w", killErr)
	}
	if !waited {
		waitErr = waitForLimitHookProcess(waitDone)
	}
	if err := waitForLimitHookProcessGroupExit(pgid); err != nil {
		return err
	}
	return waitErr
}

func waitForLimitHookProcess(waitDone <-chan error) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		return fmt.Errorf("limit hook process did not exit after SIGKILL")
	}
}

func waitForLimitHookProcessGroupExit(pgid int) error {
	return waitForLimitHookProcessGroupExitWithin(pgid, 2*time.Second)
}

func waitForLimitHookProcessGroupExitWithin(pgid int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("inspect limit hook process group: %w", err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("limit hook process group %d did not drain after SIGKILL", pgid)
		}
	}
}

func formatOptionalUint64(value *uint64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatUint(*value, 10)
}

func formatOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func formatOptionalBool(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func newLimitHookHTTPRequestExecutor() func(context.Context, string, limitHookEvent) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
	return func(ctx context.Context, endpoint string, event limitHookEvent) error {
		return postLimitHookWithClient(ctx, client, endpoint, event)
	}
}

func postLimitHook(ctx context.Context, endpoint string, event limitHookEvent) error {
	return newLimitHookHTTPRequestExecutor()(ctx, endpoint, event)
}

func postLimitHookWithClient(ctx context.Context, client *http.Client, endpoint string, event limitHookEvent) error {
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

	resp, err := client.Do(req)
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
