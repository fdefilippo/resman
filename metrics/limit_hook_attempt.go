package metrics

// RecordLimitHookAttempt counts an actual call separately from terminal jobs.
func (exp *PrometheusExporter) RecordLimitHookAttempt(hookType LimitHookType, outcome LimitHookOutcome) {
	if exp == nil || exp.limitHookAttempts == nil {
		return
	}
	switch hookType {
	case LimitHookTypeScript, LimitHookTypeHTTP:
	default:
		return
	}
	switch outcome {
	case LimitHookOutcomeSuccess, LimitHookOutcomeFailure, LimitHookOutcomeTimeout, LimitHookOutcomeCancelled:
	default:
		return
	}
	exp.limitHookAttempts.WithLabelValues(string(hookType), string(outcome)).Inc()
}
