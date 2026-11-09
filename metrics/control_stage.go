package metrics

import "time"

// ControlCycleStage identifies one of the fixed control-pipeline boundaries.
type ControlCycleStage string

const (
	StageReconcileCPUPoints             ControlCycleStage = "reconcile_cpu_points"
	StageCheckBlackout                  ControlCycleStage = "check_blackout"
	StageCollectMetrics                 ControlCycleStage = "collect_metrics"
	StageReconcileIOWeights             ControlCycleStage = "reconcile_io_weights"
	StageMakeDecision                   ControlCycleStage = "make_decision"
	StageExecuteDecision                ControlCycleStage = "execute_decision"
	StageFinalizeEnforcementObservation ControlCycleStage = "finalize_enforcement_observation"
	StageDispatchLimitHooks             ControlCycleStage = "dispatch_limit_hooks"
	StageUpdatePrometheus               ControlCycleStage = "update_prometheus"
	StageWriteDatabase                  ControlCycleStage = "write_database"
	StageRecordHistory                  ControlCycleStage = "record_history"
	StageWorkloadPatternDetection       ControlCycleStage = "workload_pattern_detection"
	StageLogCompletion                  ControlCycleStage = "log_completion"
)

// ControlStageOutcome describes the terminal outcome of an executed stage.
type ControlStageOutcome string

const (
	StageSucceeded ControlStageOutcome = "success"
	StageFailed    ControlStageOutcome = "error"
	StageSuspended ControlStageOutcome = "suspended"
	StageCancelled ControlStageOutcome = "cancelled"
)

// ControlStageObservation measures only an executed stage, never skipped work.
type ControlStageObservation struct {
	Stage    ControlCycleStage
	Outcome  ControlStageOutcome
	Duration time.Duration
}

// valid restricts stage labels to the shipped inventory.
func (s ControlCycleStage) valid() bool {
	switch s {
	case StageReconcileCPUPoints, StageCheckBlackout, StageCollectMetrics, StageReconcileIOWeights, StageMakeDecision, StageExecuteDecision, StageFinalizeEnforcementObservation, StageDispatchLimitHooks, StageUpdatePrometheus, StageWriteDatabase, StageRecordHistory, StageWorkloadPatternDetection, StageLogCompletion:
		return true
	}
	return false
}
