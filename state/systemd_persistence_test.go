package state

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type accountingSystemdAdapter struct {
	*fakeSystemdCPUUnitAdapter
	values        map[string]systemdunit.UnitAccounting
	errors        map[string]error
	coverage      map[uint32]bool
	coverageError error
	readHook      func(systemdunit.UnitIdentity)
}

func (a *accountingSystemdAdapter) ObserveAccounting(_ context.Context, identity systemdunit.UnitIdentity) (systemdunit.UnitAccounting, error) {
	if a.readHook != nil {
		a.readHook(identity)
	}
	if err := a.errors[identity.Name]; err != nil {
		return systemdunit.UnitAccounting{}, err
	}
	return a.values[identity.Name], nil
}
func (a *accountingSystemdAdapter) ObserveCPUCoverage(context.Context, systemdunit.TopologySnapshot) (map[uint32]bool, error) {
	return a.coverage, a.coverageError
}

type persistenceObservationLogger struct {
	failingCompletionLogger
	warns      []string
	warnFields [][]interface{}
	infos      []string
}

func (l *persistenceObservationLogger) Warn(message string, fields ...interface{}) {
	l.warns = append(l.warns, message)
	l.warnFields = append(l.warnFields, append([]interface{}(nil), fields...))
}

func (l *persistenceObservationLogger) Info(message string, _ ...interface{}) {
	l.infos = append(l.infos, message)
}

type forbiddenAccountingCgroupManager struct{ mockCgroupManager }

func (*forbiddenAccountingCgroupManager) GetCPUPointsNodeSnapshot(string) (cgroup.CPUPointsNodeSnapshot, error) {
	panic("systemd accounting entered migration CPU reader")
}
func (*forbiddenAccountingCgroupManager) GetMemoryAccountingSnapshot(int) (cgroup.MemoryAccountingSnapshot, error) {
	panic("systemd accounting entered migration RAM reader")
}

func accountingManager(t *testing.T) (*Manager, *accountingSystemdAdapter) {
	t.Helper()
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {1000, 300}})
	base := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000, 1001)}
	m := testSystemdCPUPointsManager(t, policy, base, &forbiddenAccountingCgroupManager{}, 4)
	m.GetConfig().UserIncludeList = []string{"alice"}
	if err := m.activateSystemdCPUPoints(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}
	a := &accountingSystemdAdapter{fakeSystemdCPUUnitAdapter: base, values: make(map[string]systemdunit.UnitAccounting), errors: make(map[string]error), coverage: map[uint32]bool{0: true, 1000: true, 1001: true}}
	for _, identity := range topologyIdentities(base.topology) {
		weight := uint64(3300)
		quota := "max 100000"
		if identity.Name == "user-1000.slice" {
			weight = 9900
		}
		if identity.IsParentUserSlice() {
			quota = "360000 100000"
		}
		cpu := cpuPersistenceSnapshot(cgroup.CgroupIdentity{Device: 1, Inode: identity.ControlGroupID}, 100, 10, 2, 3, quota, weight)
		memory := memoryPersistenceSnapshot(cpu.Identity, 96<<20, 10, 2, 1, 1)
		a.values[identity.Name] = systemdunit.UnitAccounting{Identity: identity, CPU: &cpu, Memory: &memory}
	}
	m.systemdUnits = a
	m.resourceLimits[1000] = userResourceLimitState{ramApplied: true, ioApplied: true, ramAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete}, ioAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete}}
	m.systemdResourceUnits[1000] = base.topology.Users[1].Unit.Identity
	return m, a
}

func TestSystemdAccountingRoundTripUsesSlicesAndFlatDenominator(t *testing.T) {
	m, a := accountingManager(t)
	start := time.Now().UTC()
	m.collectPersistenceInterval(persistenceSample(start))
	for name, value := range a.values {
		cpu := *value.CPU
		cpu.CPUStat.UsageUsec += 100
		cpu.CPUStat.NrPeriods += 10
		value.CPU = &cpu
		memory := *value.Memory
		memory.Events.High += 7
		value.Memory = &memory
		a.values[name] = value
	}
	sample := persistenceSample(start.Add(30 * time.Second))
	m.collectPersistenceInterval(sample)
	if sample.CPUPointsSystem.DenominatorState != resmanmetrics.CPUPointsDenominatorComplete {
		t.Fatalf("denominator: %+v", sample.CPUPointsSystem)
	}
	assertUint64Pointer(t, "root points", sample.CPUPointsSystem.ConfiguredRootPoints, 100)
	assertUint64Pointer(t, "programmed siblings", sample.CPUPointsSystem.ProgrammedSiblingWeightSum, 16500)
	assertUint64Pointer(t, "observed siblings", sample.CPUPointsSystem.ObservedSiblingWeightSum, 16500)
	assertUint64Pointer(t, "best effort", sample.CPUPointsSystem.ProgrammedBestEffortWeight, 3300)
	user := sample.CPUPointsUsers[1000]
	assertUint64Pointer(t, "RAM", user.RAMCgroupUsageBytes, 96<<20)
	assertUint64Pointer(t, "high events", user.MemoryHighEventsDelta, 7)
	if !user.CompleteUIDWorkloadGuaranteed || user.RAMCoverage == nil || *user.RAMCoverage != "complete" || user.IOCoverage == nil || *user.IOCoverage != "complete" {
		t.Fatalf("coverage: %+v", user)
	}
	if sample.CPUPointsUsers[0].ConfiguredClass != "root" || len(sample.PersistenceUsers) != 3 {
		t.Fatalf("root or excluded sibling absent: %+v", sample.PersistenceUsers)
	}
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	writer := resmanmetrics.NewDBWriter(db, 1)
	if err := writer.WriteMetricsBatch(resmanmetrics.PersistenceBatch{System: sample.PersistenceSystem, Users: sample.PersistenceUsers}); err != nil {
		t.Fatal(err)
	}
	systems, err := db.GetSystemHistory(start.Add(-time.Second), start.Add(time.Minute), 10)
	if err != nil || len(systems) != 1 {
		t.Fatalf("history: %v %v", systems, err)
	}
	if systems[0].DenominatorState != "complete" || systems[0].EnforcementMode != "systemd_native" {
		t.Fatalf("stored state: %+v", systems[0])
	}
	assertUint64Pointer(t, "stored root", systems[0].ConfiguredRootPoints, 100)
	users, err := db.GetUserHistory(1000, start.Add(-time.Second), start.Add(time.Minute), 10)
	if err != nil || len(users) != 1 {
		t.Fatalf("user history: %v %v", users, err)
	}
	assertUint64Pointer(t, "stored RAM", users[0].RAMCgroupUsageBytes, 96<<20)
	rootRows, err := db.GetUserHistory(0, start.Add(-time.Second), start.Add(time.Minute), 10)
	if err != nil || len(rootRows) != 1 || !rootRows[0].ProcessObservationUnavailable {
		t.Fatalf("unobserved root process sample: %+v %v", rootRows, err)
	}
	rootSummary, err := db.GetUserSummary(0, start.Add(-time.Second), start.Add(time.Minute))
	if err != nil || rootSummary != nil {
		t.Fatalf("unobserved root manufactured a process summary: %+v %v", rootSummary, err)
	}
	if users[0].CPUAuthorityCoverage == nil || *users[0].CPUAuthorityCoverage != "complete" || users[0].IOCoverage == nil || *users[0].IOCoverage != "complete" {
		t.Fatalf("stored coverage: %+v", users[0])
	}
}

func TestSystemdObservationFailureDegradesCyclePrometheusMCPAndPersistence(t *testing.T) {
	m, adapter := accountingManager(t)
	m.mu.Lock()
	m.systemdCPURequested = false
	m.systemdCPUComplete = false
	m.limitsActive = false
	m.mu.Unlock()
	logger := &persistenceObservationLogger{}
	m.logger = logger
	adapter.errors["user-1000.slice"] = &systemdunit.AdapterError{
		Reason:    systemdunit.ReasonMalformedReply,
		Operation: "read_slice",
		Unit:      "user-1000.slice",
		Err:       errors.New("ControlGroupId is absent or zero"),
	}
	run := &controlCycleContext{
		ctx:       context.Background(),
		cfg:       m.GetConfig(),
		cycleID:   1,
		trigger:   ControlCycleTriggerManual,
		startTime: time.Now(),
		enforcementState: cgroup.EnforcementCycleState{
			Mode:            cgroup.EnforcementModeSystemdNative,
			RequestedIntent: cgroup.EnforcementPolicyIntentMaintain,
			AppliedAction:   cgroup.AppliedEnforcementActionMaintain,
			BlockReason:     cgroup.EnforcementBlockReasonNone,
		},
	}
	if err := m.stageCollectMetrics(run); err != nil {
		t.Fatalf("stageCollectMetrics() error: %v", err)
	}
	if len(run.degradedWarnings) == 0 {
		t.Fatal("systemd observation failure published zero degraded warnings")
	}
	if len(logger.warns) != 1 {
		t.Fatalf("systemd observation failure emitted %d operator warnings, want 1", len(logger.warns))
	}
	warningFields := logFieldsByKey(t, logger.warnFields[0])
	if warningFields["error_type"] != metricsDatabaseCPUPointsReadFailure || warningFields["reason"] != string(systemdunit.ReasonMalformedReply) || warningFields["uid"] != 1000 {
		t.Fatalf("operator warning fields = %v", warningFields)
	}
	if !run.metrics.PersistenceSystem.CPUPointsDegraded || !run.metrics.CPUPointsSystem.ReconciliationDegraded {
		t.Fatalf("collection published a non-degraded observation: persistence=%t snapshot=%t", run.metrics.PersistenceSystem.CPUPointsDegraded, run.metrics.CPUPointsSystem.ReconciliationDegraded)
	}
	var observationFailure persistenceObservationFailure
	if !errors.As(run.degradedWarnings[0], &observationFailure) || observationFailure.ErrorType != metricsDatabaseCPUPointsReadFailure || observationFailure.UID != 1000 {
		t.Fatalf("cycle warning = %#v, want typed CPU observation failure for UID 1000", run.degradedWarnings[0])
	}
	var adapterError *systemdunit.AdapterError
	if !errors.As(run.degradedWarnings[0], &adapterError) || adapterError.Reason != systemdunit.ReasonMalformedReply {
		t.Fatalf("cycle warning = %v, want malformed_reply adapter reason", run.degradedWarnings[0])
	}
	if err := m.stageFinalizeEnforcementObservation(run); err != nil {
		t.Fatalf("stageFinalizeEnforcementObservation() error: %v", err)
	}
	if err := m.stageUpdatePrometheus(run); err != nil {
		t.Fatalf("stageUpdatePrometheus() error: %v", err)
	}
	if err := m.stageLogCompletion(run); err != nil {
		t.Fatalf("stageLogCompletion() error: %v", err)
	}

	fields := logFieldsByKey(t, logger.fields)
	if fields["outcome"] != "degraded" || fields["degraded_warning_count"] == 0 {
		t.Fatalf("completion outcome=%#v degraded_warning_count=%#v, want degraded and non-zero", fields["outcome"], fields["degraded_warning_count"])
	}
	if fields["enforcement_mode"] != cgroup.EnforcementModeSystemdNative {
		t.Fatalf("completion enforcement_mode=%#v, want systemd_native", fields["enforcement_mode"])
	}
	if !run.metrics.PersistenceSystem.CPUPointsDegraded || !run.metrics.CPUPointsSystem.ReconciliationDegraded {
		t.Fatalf("observation failure remained non-degraded: persistence=%t snapshot=%t", run.metrics.PersistenceSystem.CPUPointsDegraded, run.metrics.CPUPointsSystem.ReconciliationDegraded)
	}
	status := m.GetStatus()
	if status.EnforcementMode != cgroup.EnforcementModeSystemdNative || !status.CPUPoints.ReconciliationDegraded {
		t.Fatalf("MCP source status = mode=%s degraded=%t, want systemd_native and degraded", status.EnforcementMode, status.CPUPoints.ReconciliationDegraded)
	}
	exported := m.prometheusExporter.(*mockPrometheusExporter).snapshot().lastSystemSnapshot
	if exported.EnforcementMode != cgroup.EnforcementModeSystemdNative || exported.CPUPoints == nil || !exported.CPUPoints.ReconciliationDegraded {
		t.Fatalf("Prometheus snapshot = mode=%s cpu_points=%+v, want systemd_native and degraded", exported.EnforcementMode, exported.CPUPoints)
	}

	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := resmanmetrics.NewDBWriter(db, 0).WriteMetricsBatch(resmanmetrics.PersistenceBatch{
		System: run.metrics.PersistenceSystem,
		Users:  run.metrics.PersistenceUsers,
	}); err != nil {
		t.Fatal(err)
	}
	history, err := db.GetSystemHistory(run.metrics.Timestamp.Add(-time.Second), run.metrics.Timestamp.Add(time.Second), 1)
	if err != nil || len(history) != 1 || !history[0].CPUPointsDegraded {
		t.Fatalf("stored system history = %+v, error=%v, want one degraded row", history, err)
	}
}

func TestSystemdObservationFailureLoggingIsBoundedAndTopologySensitive(t *testing.T) {
	m, adapter := accountingManager(t)
	logger := &persistenceObservationLogger{}
	m.logger = logger
	observationError := &systemdunit.AdapterError{
		Reason:    systemdunit.ReasonMalformedReply,
		Operation: "read_slice",
		Unit:      "user-1000.slice",
		Err:       errors.New("ControlGroupId is absent or zero"),
	}
	adapter.errors["user-1000.slice"] = observationError
	start := time.Now().UTC()
	collect := func(at time.Time) {
		sample := persistenceSample(at)
		m.collectPersistenceInterval(sample)
		m.reportPersistenceObservationFailures(sample)
	}
	for offset := 0; offset < 3; offset++ {
		collect(start.Add(time.Duration(offset) * time.Second))
	}
	if len(logger.warns) != 1 {
		t.Fatalf("identical observation failures emitted %d warnings, want 1", len(logger.warns))
	}

	adapter.topology.Users[1].Unit.Identity.ControlGroupID++
	collect(start.Add(3 * time.Second))
	if len(logger.warns) != 2 {
		t.Fatalf("changed topology emitted %d warnings, want 2", len(logger.warns))
	}
	fields := logFieldsByKey(t, logger.warnFields[1])
	if fields["error_type"] != metricsDatabaseCPUPointsReadFailure || fields["reason"] != string(systemdunit.ReasonMalformedReply) || fields["uid"] != 1000 {
		t.Fatalf("changed-topology warning fields = %v", fields)
	}

	delete(adapter.errors, "user-1000.slice")
	collect(start.Add(4 * time.Second))
	collect(start.Add(5 * time.Second))
	if !reflect.DeepEqual(logger.infos, []string{"Typed enforcement accounting recovered"}) {
		t.Fatalf("recovery messages = %v, want one", logger.infos)
	}

	adapter.errors["user-1000.slice"] = observationError
	collect(start.Add(6 * time.Second))
	if len(logger.warns) != 3 {
		t.Fatalf("failure after recovery emitted %d warnings, want 3 total", len(logger.warns))
	}
}

func TestSystemdResourceCoverageIsPublishedFromTheCurrentControlCycle(t *testing.T) {
	m, a := accountingManager(t)
	m.cfg.RAMEnabled = true
	m.cfg.IOEnabled = true
	m.resolveSystemdIODevices = func(string) ([]string, error) { return []string{"/dev/vda"}, nil }
	m.systemdResourcesRequested = true

	start := time.Now().UTC()
	m.collectPersistenceInterval(persistenceSample(start))
	partial := systemdunit.ResourceAuthority{State: systemdunit.ResourceCoveragePartial, Reason: systemdunit.ResourceCoverageAuthoritySplit}
	a.authority = map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{
		systemdunit.ResourceMemory: partial,
		systemdunit.ResourceIO:     partial,
	}
	a.authorityError = map[systemdunit.ResourceKind]error{
		systemdunit.ResourceMemory: &systemdunit.ResourceAuthorityError{UID: 1000, Authority: partial},
		systemdunit.ResourceIO:     &systemdunit.ResourceAuthorityError{UID: 1000, Authority: partial},
	}

	refused := persistenceSample(start.Add(30 * time.Second))
	refused.RAMEligibleUsers = []int{1000}
	refused.IOEligibleUsers = []int{1000}
	m.collectPersistenceInterval(refused)
	if got := refused.PersistenceUsers[1000]; got.RAMCoverage == nil || *got.RAMCoverage != "complete" || got.IOCoverage == nil || *got.IOCoverage != "complete" {
		t.Fatalf("pre-decision coverage = %+v, want previous-cycle complete state", got)
	}
	run := &controlCycleContext{metrics: refused, decision: "MAINTAIN_CURRENT_STATE"}
	err := runControlCyclePipeline(m, run, []controlCycleStage{
		{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
		{name: "finalize_enforcement_observation", run: (*Manager).stageFinalizeEnforcementObservation},
		{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
	})
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Step != "authority" {
		t.Fatalf("refused cycle error = %v, want typed authority refusal", err)
	}
	assertResourceCoverage(t, "refused persistence", refused.PersistenceUsers[1000].RAMCoverage, refused.PersistenceUsers[1000].IOCoverage, "partial")
	assertResourceCoverage(t, "refused operational", refused.CPUPointsUsers[1000].RAMCoverage, refused.CPUPointsUsers[1000].IOCoverage, "partial")
	exporter := m.prometheusExporter.(*mockPrometheusExporter)
	assertResourceCoverage(t, "refused Prometheus", exporter.userSnapshots[1000].CPUPoints.RAMCoverage, exporter.userSnapshots[1000].CPUPoints.IOCoverage, "partial")
	assertStoredResourceCoverage(t, refused, "partial")

	complete := systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete, Reason: systemdunit.ResourceCoverageVerified}
	a.authority = map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{
		systemdunit.ResourceMemory: complete,
		systemdunit.ResourceIO:     complete,
	}
	a.authorityError = nil
	m.systemdResourcesRequested = true
	recovered := persistenceSample(start.Add(60 * time.Second))
	recovered.RAMEligibleUsers = []int{1000}
	recovered.IOEligibleUsers = []int{1000}
	m.collectPersistenceInterval(recovered)
	if got := recovered.PersistenceUsers[1000]; got.RAMCoverage == nil || *got.RAMCoverage != "partial" || got.IOCoverage == nil || *got.IOCoverage != "partial" {
		t.Fatalf("next pre-decision coverage = %+v, want previous-cycle refusal", got)
	}
	run = &controlCycleContext{metrics: recovered, decision: "MAINTAIN_CURRENT_STATE"}
	if err := runControlCyclePipeline(m, run, []controlCycleStage{
		{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
		{name: "finalize_enforcement_observation", run: (*Manager).stageFinalizeEnforcementObservation},
		{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
	}); err != nil {
		t.Fatalf("recovered cycle error: %v", err)
	}
	assertResourceCoverage(t, "recovered persistence", recovered.PersistenceUsers[1000].RAMCoverage, recovered.PersistenceUsers[1000].IOCoverage, "complete")
	assertResourceCoverage(t, "recovered Prometheus", exporter.userSnapshots[1000].CPUPoints.RAMCoverage, exporter.userSnapshots[1000].CPUPoints.IOCoverage, "complete")
	assertStoredResourceCoverage(t, recovered, "complete")
}

func TestSystemdResourceApplyFailurePublishesRefusedCoverageInTheCurrentCycle(t *testing.T) {
	for _, resource := range []systemdunit.ResourceKind{systemdunit.ResourceMemory, systemdunit.ResourceIO} {
		t.Run(string(resource), func(t *testing.T) {
			m, a := accountingManager(t)
			m.systemdResourcesRequested = true
			delete(m.resourceLimits, 1000)
			delete(m.systemdResourceUnits, 1000)
			a.failApplyUnit = "user-1000.slice"

			sample := persistenceSample(time.Now().UTC())
			switch resource {
			case systemdunit.ResourceMemory:
				m.cfg.RAMEnabled = true
				sample.RAMEligibleUsers = []int{1000}
			case systemdunit.ResourceIO:
				m.cfg.IOEnabled = true
				m.resolveSystemdIODevices = func(string) ([]string, error) { return []string{"/dev/vda"}, nil }
				sample.IOEligibleUsers = []int{1000}
			}
			m.collectPersistenceInterval(sample)
			run := &controlCycleContext{metrics: sample, decision: "MAINTAIN_CURRENT_STATE"}
			err := runControlCyclePipeline(m, run, []controlCycleStage{
				{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
				{name: "finalize_enforcement_observation", run: (*Manager).stageFinalizeEnforcementObservation},
				{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
			})
			var reconciliationErr *SystemdResourceReconciliationError
			if !errors.As(err, &reconciliationErr) || reconciliationErr.Resource != resource || reconciliationErr.Step != "apply" {
				t.Fatalf("failed apply error = %v, want typed %s apply failure", err, resource)
			}

			persisted := sample.PersistenceUsers[1000]
			status, ok := m.GetCPUPointsUserStatus(1000)
			if !ok {
				t.Fatal("failed apply omitted the MCP user status")
			}
			exported := m.prometheusExporter.(*mockPrometheusExporter).userSnapshots[1000].CPUPoints
			refused := "refused"
			var authority *systemdunit.ResourceAuthority
			var persistedCoverage, statusCoverage, exportedCoverage *string
			var wantRAM, wantIO *string
			if resource == systemdunit.ResourceMemory {
				authority = sample.systemdRAMAuthority[1000]
				persistedCoverage, statusCoverage, exportedCoverage = persisted.RAMCoverage, status.RAMCoverage, exported.RAMCoverage
				wantRAM = &refused
			} else {
				authority = sample.systemdIOAuthority[1000]
				persistedCoverage, statusCoverage, exportedCoverage = persisted.IOCoverage, status.IOCoverage, exported.IOCoverage
				wantIO = &refused
			}
			if authority == nil || authority.State != systemdunit.ResourceCoverageRefused || authority.Reason != systemdunit.ResourceCoverageApplyFailed {
				t.Fatalf("failed apply cycle authority = %+v, want refused/apply_failed", authority)
			}
			for name, coverage := range map[string]*string{"persistence": persistedCoverage, "MCP": statusCoverage, "Prometheus": exportedCoverage} {
				if coverage == nil || *coverage != refused {
					t.Fatalf("failed apply %s coverage = %v, want refused", name, coverage)
				}
			}
			assertStoredCoverage(t, sample, wantRAM, wantIO)
		})
	}
}

func assertResourceCoverage(t *testing.T, name string, ram, io *string, want string) {
	t.Helper()
	if ram == nil || *ram != want || io == nil || *io != want {
		t.Fatalf("%s RAM=%v I/O=%v, want %q", name, ram, io, want)
	}
}

func assertStoredResourceCoverage(t *testing.T, sample *SystemMetrics, want string) {
	t.Helper()
	assertStoredCoverage(t, sample, &want, &want)
}

func assertStoredCoverage(t *testing.T, sample *SystemMetrics, wantRAM, wantIO *string) {
	t.Helper()
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := resmanmetrics.NewDBWriter(db, 1).WriteMetricsBatch(resmanmetrics.PersistenceBatch{System: sample.PersistenceSystem, Users: sample.PersistenceUsers}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.GetUserHistory(1000, sample.Timestamp.Add(-time.Second), sample.Timestamp.Add(time.Second), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored coverage rows=%v error=%v", rows, err)
	}
	for name, pair := range map[string]struct {
		got  *string
		want *string
	}{
		"RAM": {got: rows[0].RAMCoverage, want: wantRAM},
		"I/O": {got: rows[0].IOCoverage, want: wantIO},
	} {
		if pair.want == nil {
			if pair.got != nil {
				t.Fatalf("stored %s coverage = %v, want nil", name, pair.got)
			}
			continue
		}
		if pair.got == nil || *pair.got != *pair.want {
			t.Fatalf("stored %s coverage = %v, want %q", name, pair.got, *pair.want)
		}
	}
}

func TestSystemdAccountingDoesNotInventCompleteObservations(t *testing.T) {
	for _, fault := range []string{"missing_cpu", "missing_memory", "external_weight", "recreated_unit", "parent_recreated", "parent_quota_mismatch", "late_topology", "incomplete_plan", "authority_split", "missing_coverage", "capacity_unavailable"} {
		t.Run(fault, func(t *testing.T) {
			m, a := accountingManager(t)
			start := time.Now()
			m.collectPersistenceInterval(persistenceSample(start))
			value := a.values["user-1000.slice"]
			switch fault {
			case "missing_cpu":
				value.CPU = nil
			case "missing_memory":
				value.Memory = nil
				value.MemoryError = errors.New("unreadable memory")
			case "external_weight":
				cpu := *value.CPU
				cpu.CPUWeight = 1
				value.CPU = &cpu
			case "recreated_unit":
				a.topology.Users[1].Unit.Identity.InvocationID[0]++
			case "parent_recreated":
				a.topology.Parent.Identity.InvocationID[0]++
			case "parent_quota_mismatch":
				parent := a.values["user.slice"]
				cpu := *parent.CPU
				cpu.CPUQuota = "350000 100000"
				parent.CPU = &cpu
				a.values["user.slice"] = parent
			case "late_topology":
				a.confirmError = errors.New("late topology change")
			case "incomplete_plan":
				m.systemdCPUComplete = false
			case "authority_split":
				a.coverage[1000] = false
			case "missing_coverage":
				a.coverage = nil
			case "capacity_unavailable":
				m.cpuCapacity.(*mutableCPUCapacityProvider).state.Available = false
			}
			a.values["user-1000.slice"] = value
			sample := persistenceSample(start.Add(30 * time.Second))
			m.collectPersistenceInterval(sample)
			user := sample.CPUPointsUsers[1000]
			if fault == "missing_memory" {
				if user.RAMCgroupUsageBytes != nil || user.MemoryHighEventsDelta != nil || user.RAMCoverage == nil || *user.RAMCoverage != "unavailable" {
					t.Fatalf("invented RAM: %+v", user)
				}
				return
			}
			if user.CompleteUIDWorkloadGuaranteed {
				t.Fatalf("invented guarantee: %+v", user)
			}
			if fault == "recreated_unit" && (user.RAMCoverage == nil || *user.RAMCoverage != "unavailable" || user.IOCoverage == nil || *user.IOCoverage != "unavailable") {
				t.Fatalf("resource authority inherited from an earlier unit lifetime: %+v", user)
			}
			if fault != "authority_split" && fault != "missing_coverage" && sample.CPUPointsSystem.DenominatorState == resmanmetrics.CPUPointsDenominatorComplete {
				t.Fatal("invented complete denominator")
			}
		})
	}
}

func TestSystemdAccountingDoesNotCarryMemoryDeltasAcrossUnitLifetimes(t *testing.T) {
	m, a := accountingManager(t)
	start := time.Now().UTC()
	m.collectPersistenceInterval(persistenceSample(start))

	value := a.values["user-1000.slice"]
	memory := *value.Memory
	memory.Events.High += 7
	value.Memory = &memory
	a.values["user-1000.slice"] = value
	// Model a recreated systemd unit whose cgroup inode has been reused.
	a.topology.Users[1].Unit.Identity.InvocationID[0]++

	sample := persistenceSample(start.Add(30 * time.Second))
	m.collectPersistenceInterval(sample)
	user := sample.CPUPointsUsers[1000]
	if user.RAMCgroupUsageBytes == nil {
		t.Fatalf("current memory observation was lost: %+v", user)
	}
	if user.MemoryHighEventsDelta != nil {
		t.Fatalf("memory delta crossed a recreated unit lifetime: %+v", user)
	}
}

func TestSystemdAccountingResetsOnlyUnavailableOrRecreatedBaselines(t *testing.T) {
	for _, fault := range []string{"missing", "unit_recreated", "cgroup_recreated", "counter_decreased"} {
		t.Run(fault, func(t *testing.T) {
			m, a := accountingManager(t)
			start := time.Now()
			m.collectPersistenceInterval(persistenceSample(start))
			value := a.values["user-1000.slice"]
			cpu := *value.CPU
			cpu.CPUStat.UsageUsec = 300
			value.CPU = &cpu
			switch fault {
			case "missing":
				a.values["user-1000.slice"] = systemdunit.UnitAccounting{}
				m.collectPersistenceInterval(persistenceSample(start.Add(10 * time.Second)))
			case "unit_recreated":
				a.topology.Users[1].Unit.Identity.InvocationID[0]++
			case "cgroup_recreated":
				cpu.Identity.Inode++
			case "counter_decreased":
				cpu.CPUStat.UsageUsec = 1
			}
			a.values["user-1000.slice"] = value
			sample := persistenceSample(start.Add(30 * time.Second))
			m.collectPersistenceInterval(sample)
			if sample.CPUPointsUsers[1000].LeafCPUUsageUsecDelta != nil {
				t.Fatalf("invalid baseline: %+v", sample.CPUPointsUsers[1000])
			}
		})
	}
}

func TestSystemdAccountingPreservesProcessesWithoutUserSlices(t *testing.T) {
	for _, scenario := range []string{"process_only", "root_process_only", "slice_departed", "discovery_unavailable", "ineligible"} {
		t.Run(scenario, func(t *testing.T) {
			m, a := accountingManager(t)
			start := time.Now().UTC()
			m.collectPersistenceInterval(persistenceSample(start))
			sample := persistenceSample(start.Add(30 * time.Second))
			uid := 1002
			if scenario == "root_process_only" {
				uid = 0
				a.topology.Users = a.topology.Users[1:]
			}
			if scenario == "slice_departed" {
				uid = 1000
				a.topology.Users = append(a.topology.Users[:1], a.topology.Users[2:]...)
			}
			if scenario == "discovery_unavailable" {
				a.discoverError = errors.New("discovery unavailable")
			}
			observed := &resmanmetrics.UserMetrics{UID: uid, Username: "service", CPUUsage: 40, MemoryUsage: 32 << 20, ProcessCount: 3, EligibleForCPU: scenario != "ineligible", CPULimitRequested: true}
			sample.UserMetrics[uid] = observed
			m.collectPersistenceInterval(sample)
			user, exists := sample.PersistenceUsers[uid]
			if !exists || user.Metrics != observed || user.ProcessObservationUnavailable {
				t.Fatalf("observed UID lost: %+v", sample.PersistenceUsers)
			}
			wantLifecycle := resmanmetrics.CPUPointsLifecycleEligibleInactive
			if scenario == "ineligible" {
				wantLifecycle = resmanmetrics.CPUPointsLifecycleIneligible
			}
			if scenario == "discovery_unavailable" {
				wantLifecycle = resmanmetrics.CPUPointsLifecycleFailed
			}
			if user.LifecycleState != wantLifecycle || user.AppliedClass != nil || user.AppliedWeight != nil || user.CPUWeight != nil || user.LeafCPUUsageUsecDelta != nil || user.RAMCgroupUsageBytes != nil || user.MemoryHighLimit != nil || user.MemoryHighEventsDelta != nil || user.CgroupPath != "" || user.CPUQuota != "" {
				t.Fatalf("slice observation invented or retained: %+v", user)
			}
			for _, coverage := range []*string{user.CPUAuthorityCoverage, user.RAMCoverage, user.IOCoverage} {
				if coverage == nil || *coverage != "unavailable" {
					t.Fatalf("invented resource coverage: %+v", user)
				}
			}
			if scenario == "slice_departed" && (user.ConfiguredClass != "guaranteed" || user.ConfiguredGuaranteePoints == nil || *user.ConfiguredGuaranteePoints != 300) {
				t.Fatalf("configuration lost with slice: %+v", user)
			}
			if scenario == "root_process_only" && user.ConfiguredClass != "root" {
				t.Fatalf("sliceless root was not classified as root: %+v", user)
			}
			status, exists := m.GetCPUPointsUserStatus(uid)
			if !exists || status.ObservedProcessCount != 3 || !status.CPUEnforcementRequested || status.AppliedToProcesses || status.CompleteUIDWorkloadGuaranteed || status.ProcessCoverage != resmanmetrics.CPUPointsCoverageUnavailable {
				t.Fatalf("typed status disagrees with persistence: %+v", status)
			}
			if scenario == "process_only" {
				if len(sample.PersistenceUsers) != 4 || sample.CPUPointsSystem.DenominatorState != resmanmetrics.CPUPointsDenominatorComplete {
					t.Fatalf("union or denominator changed: %+v", sample.CPUPointsSystem)
				}
				assertUint64Pointer(t, "unchanged sibling weight", sample.CPUPointsSystem.ProgrammedSiblingWeightSum, 16500)
			}
			db, err := database.NewDatabaseManager(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := resmanmetrics.NewDBWriter(db, 0).WriteMetricsBatch(resmanmetrics.PersistenceBatch{System: sample.PersistenceSystem, Users: sample.PersistenceUsers}); err != nil {
				t.Fatal(err)
			}
			rows, err := db.GetUserHistory(uid, start, sample.Timestamp.Add(time.Second), 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("history missing: %+v %v", rows, err)
			}
			row := rows[0]
			if row.ProcessObservationUnavailable || row.CPUUsagePercent != 40 || row.ProcessCount != 3 || row.MemoryUsageBytes != 32<<20 || row.EligibleForCPU != observed.EligibleForCPU || row.CPUWeight != nil || row.AppliedCPUWeight != nil || row.RAMCgroupUsageBytes != nil || row.CPUPointsLifecycleState != string(wantLifecycle) {
				t.Fatalf("history disagrees with observation: %+v", row)
			}
		})
	}
}

func TestPrometheusIncludesUsersObservedOnlyThroughSystemdSlices(t *testing.T) {
	m, _ := accountingManager(t)
	sample := persistenceSample(time.Now().UTC())
	m.collectPersistenceInterval(sample)
	if _, exists := sample.UserMetrics[0]; exists {
		t.Fatal("fixture unexpectedly contains a process observation for root")
	}
	if _, exists := sample.PersistenceUsers[0]; !exists {
		t.Fatal("fixture does not contain the root slice observation")
	}

	m.updatePrometheusDecisionUserMetrics(sample)
	exporter := m.prometheusExporter.(*mockPrometheusExporter)
	root, exists := exporter.userSnapshots[0]
	if !exists {
		t.Fatalf("slice-only root missing from Prometheus snapshots: %+v", exporter.userSnapshots)
	}
	if root.CPUPoints.ConfiguredClass != "root" || root.CPUPoints.ObservedWeight == nil || *root.CPUPoints.ObservedWeight != 3300 {
		t.Fatalf("slice-only root snapshot = %+v", root)
	}
}
