package state

import (
	"context"
	"errors"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/limithook"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const limitHookSaturationLogInterval = time.Minute

var errLimitHookSaturated = errors.New("limit-hook queue saturated")

type hookScriptInvocation struct {
	path           string
	identity       limithook.ScriptIdentity
	dropPrivileges bool
}

type limitHookJob struct {
	hookType resmanmetrics.LimitHookType
	script   hookScriptInvocation
	endpoint string
	timeout  time.Duration
	event    limitHookEvent
}

func (m *Manager) dispatchLimitHookJobs(cfg *config.Config, event limitHookEvent) {
	timeout := time.Duration(cfg.LimitHookTimeout) * time.Second
	if cfg.LimitHookScript != "" {
		m.enqueueLimitHookJob(limitHookJob{
			hookType: resmanmetrics.LimitHookTypeScript,
			script: hookScriptInvocation{
				path:           cfg.LimitHookScript,
				identity:       m.hookScriptIdentity,
				dropPrivileges: true,
			},
			timeout: timeout,
			event:   event,
		})
	}
	if cfg.LimitHookURL != "" {
		m.enqueueLimitHookJob(limitHookJob{
			hookType: resmanmetrics.LimitHookTypeHTTP,
			endpoint: cfg.LimitHookURL,
			timeout:  timeout,
			event:    event,
		})
	}
}

func (m *Manager) enqueueLimitHookJob(job limitHookJob) {
	m.hookMu.Lock()
	if m.hookClosed {
		m.hookMu.Unlock()
		m.recordLimitHookResult(job.event, job.hookType, job.endpoint, context.Canceled)
		return
	}
	if !m.hookWorkersStarted {
		m.startLimitHookWorkersLocked()
	}
	select {
	case m.hookQueue <- job:
		queued := len(m.hookQueue)
		m.hookMu.Unlock()
		m.observeLimitHookExecutor(queued)
	default:
		m.hookMu.Unlock()
		m.recordLimitHookResult(job.event, job.hookType, job.endpoint, errLimitHookSaturated)
	}
}

func (m *Manager) startLimitHookWorkersLocked() {
	m.hookWorkersStarted = true
	m.hookWG.Add(m.hookWorkerCount)
	for range m.hookWorkerCount {
		go m.runLimitHookWorker()
	}
}

func (m *Manager) runLimitHookWorker() {
	defer m.hookWG.Done()
	for job := range m.hookQueue {
		m.hookInFlight.Add(1)
		m.observeLimitHookExecutor(len(m.hookQueue))
		m.runLimitHookJob(job)
		m.hookInFlight.Add(-1)
		m.observeLimitHookExecutor(len(m.hookQueue))
	}
}

func (m *Manager) runLimitHookJob(job limitHookJob) {
	ctx, cancel := context.WithTimeout(m.hookCtx, job.timeout)
	defer cancel()

	var err error
	if job.hookType == resmanmetrics.LimitHookTypeScript {
		err = m.executeHookScript(ctx, job.script, job.event)
	} else {
		err = m.executeHookRequest(ctx, job.endpoint, job.event)
	}
	m.recordLimitHookResult(job.event, job.hookType, job.endpoint, err)
}

func (m *Manager) observeLimitHookExecutor(queued int) {
	if m.prometheusExporter == nil {
		return
	}
	m.prometheusExporter.ObserveLimitHookExecutor(
		int(m.hookInFlight.Load()),
		queued,
		cap(m.hookQueue),
	)
}

func (m *Manager) stopLimitHooks() {
	m.hookMu.Lock()
	if m.hookClosed {
		m.hookMu.Unlock()
		m.hookWG.Wait()
		return
	}
	m.hookClosed = true
	close(m.hookQueue)
	cancel := m.hookCancel
	m.hookMu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.hookWG.Wait()
	m.observeLimitHookExecutor(0)
}

func (m *Manager) reportLimitHookSaturation(hookType resmanmetrics.LimitHookType) {
	now := m.hookNow()
	m.hookMu.Lock()
	if !m.hookLastSaturationLog.IsZero() && now.Sub(m.hookLastSaturationLog) < limitHookSaturationLogInterval {
		m.hookSaturationSuppressed++
		m.hookMu.Unlock()
		return
	}
	suppressed := m.hookSaturationSuppressed
	m.hookSaturationSuppressed = 0
	m.hookLastSaturationLog = now
	m.hookMu.Unlock()
	m.logger.Warn("Limit hook queue saturated",
		"hook_type", hookType,
		"queue_capacity", cap(m.hookQueue),
		"suppressed_since_last", suppressed,
	)
}
