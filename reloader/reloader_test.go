/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
package reloader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/configepoch"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

type testStateConfigManager struct {
	cfg     *config.Config
	updates int
	epoch   configepoch.Barrier
}

type testCPUPointsStateManager struct {
	testStateConfigManager
	policy       cpupoints.PolicySnapshot
	reconciled   []cpupoints.PolicySnapshot
	published    int
	reconcileErr error
	onReconcile  func()
}

func (m *testCPUPointsStateManager) CurrentCPUPointsPolicy() cpupoints.PolicySnapshot {
	return m.policy
}

func (m *testCPUPointsStateManager) ReconcileCPUPointsPolicy(candidate, _ cpupoints.PolicySnapshot) error {
	m.reconciled = append(m.reconciled, candidate)
	if m.onReconcile != nil {
		m.onReconcile()
		m.onReconcile = nil
	}
	return m.reconcileErr
}

func (m *testCPUPointsStateManager) PublishCPUPointsPolicy(candidate cpupoints.PolicySnapshot) {
	m.policy = candidate
	m.published++
}

type reloaderResolver map[string]int

func (r reloaderResolver) ResolveExactUsername(username string) ([]cpupoints.ResolvedUserIdentity, error) {
	uid, ok := r[username]
	if !ok {
		return nil, fmt.Errorf("unknown test username %q", username)
	}
	return []cpupoints.ResolvedUserIdentity{{Username: username, UID: uid}}, nil
}

type testCPUPointsPreflightError struct{}

func (*testCPUPointsPreflightError) Error() string       { return "active allocation class changed" }
func (*testCPUPointsPreflightError) CPUPointsPreflight() {}

func (m *testStateConfigManager) BeginConfigUpdate() func() {
	return m.epoch.BeginUpdate()
}

func (m *testStateConfigManager) GetConfig() *config.Config {
	return m.cfg
}

func (m *testStateConfigManager) UpdateConfig(cfg *config.Config) {
	m.cfg = cfg
	m.updates++
}

type testCgroupConfigManager struct {
	cfg           *config.Config
	err           error
	updateEntered chan struct{}
	releaseUpdate chan struct{}
}

func (m *testCgroupConfigManager) UpdateConfig(cfg *config.Config) error {
	m.cfg = cfg
	if m.updateEntered != nil {
		close(m.updateEntered)
	}
	if m.releaseUpdate != nil {
		<-m.releaseUpdate
	}
	return m.err
}

type testMetricsConfigCollector struct {
	cfg *config.Config
}

func (c *testMetricsConfigCollector) UpdateConfig(cfg *config.Config) {
	c.cfg = cfg
}

func TestNewReloader(t *testing.T) {
	reloader := NewReloader(nil, nil, nil)

	if reloader == nil {
		t.Fatal("NewReloader() returned nil")
	}

	if reloader.stateManager != nil {
		t.Error("stateManager should be nil")
	}
	if reloader.cgroupManager != nil {
		t.Error("cgroupManager should be nil")
	}
	if reloader.metricsCollector != nil {
		t.Error("metricsCollector should be nil")
	}
}

func TestOnConfigChange(t *testing.T) {
	reloader := NewReloader(nil, nil, nil)

	if reloader == nil {
		t.Fatal("NewReloader() returned nil")
	}

	cfg := config.DefaultConfig()
	err := reloader.OnConfigChange(cfg)

	// Should not error with nil components
	if err != nil {
		t.Logf("OnConfigChange returned: %v", err)
	}
}

func loadReloaderPolicy(t *testing.T, path, content string) cpupoints.PolicySnapshot {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	mapPath, err := cpupoints.NewPolicyMapPath(path)
	if err != nil {
		t.Fatal(err)
	}
	reserve, _ := cpupoints.NewReservePoints(100)
	bestEffort, _ := cpupoints.NewBestEffortPoints(100)
	policy, err := cpupoints.NewPolicyLoader().Load(cpupoints.PolicyInputs{
		Reserve: reserve, BestEffort: bestEffort, MapPath: mapPath,
	}, reloaderResolver{"alice": 1000})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestCompositeReloadAppliesLifecycleBeforeSelectingPolicyMap(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.map")
	newPath := filepath.Join(dir, "new.map")
	oldPolicy := loadReloaderPolicy(t, oldPath, cpupoints.PolicyMapMarker+"\nalice=300\n")
	_ = loadReloaderPolicy(t, newPath, cpupoints.PolicyMapMarker+"\nalice=700\n")
	current := config.DefaultConfig()
	current.CPUPointsFile = oldPath
	requested := config.DefaultConfig()
	requested.CPUPointsFile = newPath
	requested.CPUReservePoints = 200
	stateManager := &testCPUPointsStateManager{
		testStateConfigManager: testStateConfigManager{cfg: current},
		policy:                 oldPolicy,
	}
	reloader := NewReloader(stateManager, nil, nil)
	reloader.identityResolver = reloaderResolver{"alice": 1000}

	outcome := reloader.OnConfigCandidate(requested, func() error { return nil })
	var restartErr *config.RestartRequiredError
	if !errors.As(outcome.Err, &restartErr) {
		t.Fatalf("OnConfigCandidate() error = %v, want RestartRequiredError", outcome.Err)
	}
	if !outcome.Published || !outcome.Processed {
		t.Fatalf("outcome = %+v, want published and processed", outcome)
	}
	if len(stateManager.reconciled) != 1 {
		t.Fatalf("reconciled candidates = %d, want 1", len(stateManager.reconciled))
	}
	if got := stateManager.reconciled[0].Source().Path().String(); got != oldPath {
		t.Fatalf("candidate map path = %s, want retained %s", got, oldPath)
	}
	if guarantee, ok := stateManager.reconciled[0].GuaranteeForUID(1000); !ok || guarantee.Points().Value() != 300 {
		t.Fatalf("candidate guarantee = %+v, %t; want old-map 300", guarantee, ok)
	}
	if stateManager.cfg.GetCPUPointsFile() != oldPath || stateManager.cfg.GetCPUReservePoints() != 200 {
		t.Fatalf("published config path/reserve = %s/%d", stateManager.cfg.GetCPUPointsFile(), stateManager.cfg.GetCPUReservePoints())
	}
}

func TestCompositeReloadRollsBackWhenMapChangesAfterReconciliation(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "cpu-points.map")
	oldPolicy := loadReloaderPolicy(t, mapPath, cpupoints.PolicyMapMarker+"\nalice=300\n")
	current := config.DefaultConfig()
	current.CPUPointsFile = mapPath
	requested := config.DefaultConfig()
	requested.CPUPointsFile = mapPath
	stateManager := &testCPUPointsStateManager{
		testStateConfigManager: testStateConfigManager{cfg: current},
		policy:                 oldPolicy,
		onReconcile: func() {
			if err := os.WriteFile(mapPath, []byte(cpupoints.PolicyMapMarker+"\nalice=400\n"), 0600); err != nil {
				t.Error(err)
			}
		},
	}
	reloader := NewReloader(stateManager, nil, nil)
	reloader.identityResolver = reloaderResolver{"alice": 1000}

	outcome := reloader.OnConfigCandidate(requested, func() error { return nil })
	if outcome.Err == nil || outcome.Published || outcome.Processed {
		t.Fatalf("outcome = %+v, want unprocessed stale-source rejection", outcome)
	}
	if len(stateManager.reconciled) != 2 {
		t.Fatalf("reconciliation calls = %d, want candidate plus old-epoch restore", len(stateManager.reconciled))
	}
	if stateManager.published != 0 || stateManager.cfg != current {
		t.Fatal("stale source published candidate configuration or policy")
	}
}

func TestCompositeReloadRollsBackWhenMapChangesDuringApplicationBeforeAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "cpu-points.map")
	oldPolicy := loadReloaderPolicy(t, mapPath, cpupoints.PolicyMapMarker+"\nalice=300\n")
	if err := os.WriteFile(mapPath, []byte(cpupoints.PolicyMapMarker+"\nalice=400\n"), 0600); err != nil {
		t.Fatal(err)
	}
	current := config.DefaultConfig()
	current.CPUPointsFile = mapPath
	requested := config.DefaultConfig()
	requested.CPUPointsFile = mapPath
	requested.CPUThreshold = 88
	stateManager := &testCPUPointsStateManager{
		testStateConfigManager: testStateConfigManager{cfg: current},
		policy:                 oldPolicy,
	}
	cgroupManager := &testCgroupConfigManager{cfg: current}
	metricsCollector := &testMetricsConfigCollector{cfg: current}
	var applyCalls []*config.Config
	reloader := NewReloader(
		stateManager,
		cgroupManager,
		metricsCollector,
		func(cfg *config.Config) error {
			applyCalls = append(applyCalls, cfg)
			if cfg != requested {
				return nil
			}
			return os.WriteFile(mapPath, []byte(cpupoints.PolicyMapMarker+"\nalice=500\n"), 0600)
		},
	)
	reloader.identityResolver = reloaderResolver{"alice": 1000}

	outcome := reloader.OnConfigCandidate(requested, func() error { return nil })
	if outcome.Err == nil || outcome.Published || outcome.Processed {
		t.Fatalf("outcome = %+v, want unprocessed stale-source rejection after application", outcome)
	}
	if len(stateManager.reconciled) != 2 {
		t.Fatalf("reconciliation calls = %d, want candidate plus old-epoch restore", len(stateManager.reconciled))
	}
	if guarantee, ok := stateManager.reconciled[0].GuaranteeForUID(1000); !ok || guarantee.Points().Value() != 400 {
		t.Fatalf("candidate guarantee = %+v, %t; want 400", guarantee, ok)
	}
	if guarantee, ok := stateManager.reconciled[1].GuaranteeForUID(1000); !ok || guarantee.Points().Value() != 300 {
		t.Fatalf("restored guarantee = %+v, %t; want 300", guarantee, ok)
	}
	if stateManager.published != 0 {
		t.Fatal("stale source published the candidate policy")
	}
	if guarantee, ok := stateManager.policy.GuaranteeForUID(1000); !ok || guarantee.Points().Value() != 300 {
		t.Fatalf("authoritative policy guarantee = %+v, %t; want old value 300", guarantee, ok)
	}
	if stateManager.updates != 2 || stateManager.cfg != current {
		t.Fatalf("state configuration updates/final = %d/%p, want 2/%p", stateManager.updates, stateManager.cfg, current)
	}
	if cgroupManager.cfg != current || metricsCollector.cfg != current {
		t.Fatal("component configuration was not rolled back to the previous epoch")
	}
	if len(applyCalls) != 2 || applyCalls[0] != requested || applyCalls[1] != current {
		t.Fatalf("application calls = %v, want requested then current configuration", applyCalls)
	}
}

func TestCompositeReloadSeparatesPreflightFromPartialMutation(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "cpu-points.map")
	oldPolicy := loadReloaderPolicy(t, mapPath, cpupoints.PolicyMapMarker+"\nalice=300\n")
	for _, test := range []struct {
		name          string
		err           error
		wantProcessed bool
	}{
		{name: "preflight", err: &testCPUPointsPreflightError{}, wantProcessed: false},
		{name: "partial mutation", err: errors.New("kernel readback failed"), wantProcessed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := config.DefaultConfig()
			current.CPUPointsFile = mapPath
			requested := config.DefaultConfig()
			requested.CPUPointsFile = mapPath
			stateManager := &testCPUPointsStateManager{
				testStateConfigManager: testStateConfigManager{cfg: current},
				policy:                 oldPolicy,
				reconcileErr:           test.err,
			}
			reloader := NewReloader(stateManager, nil, nil)
			reloader.identityResolver = reloaderResolver{"alice": 1000}
			outcome := reloader.OnConfigCandidate(requested, func() error { return nil })
			if outcome.Err == nil || outcome.Published || outcome.Processed != test.wantProcessed {
				t.Fatalf("outcome = %+v, want processed=%t and unpublished error", outcome, test.wantProcessed)
			}
			if stateManager.updates != 0 || stateManager.published != 0 {
				t.Fatal("failed reconciliation published configuration or policy")
			}
		})
	}
}

func TestCompositeReloadBlocksControlReadersUntilPolicyAndComponentsShareOneEpoch(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "cpu-points.map")
	oldPolicy := loadReloaderPolicy(t, mapPath, cpupoints.PolicyMapMarker+"\nalice=300\n")
	current := config.DefaultConfig()
	current.CPUPointsFile = mapPath
	requested := config.DefaultConfig()
	requested.CPUPointsFile = mapPath
	requested.CPUThreshold = 88
	stateManager := &testCPUPointsStateManager{
		testStateConfigManager: testStateConfigManager{cfg: current},
		policy:                 oldPolicy,
	}
	cgroupManager := &testCgroupConfigManager{updateEntered: make(chan struct{}), releaseUpdate: make(chan struct{})}
	metricsCollector := &testMetricsConfigCollector{cfg: current}
	reloader := NewReloader(stateManager, cgroupManager, metricsCollector)
	reloader.identityResolver = reloaderResolver{"alice": 1000}

	reloadDone := make(chan config.ReloadApplyOutcome, 1)
	go func() {
		reloadDone <- reloader.OnConfigCandidate(requested, func() error { return nil })
	}()
	<-cgroupManager.updateEntered
	readerDone := make(chan *config.Config, 1)
	go func() {
		leave := stateManager.epoch.Enter()
		defer leave()
		readerDone <- stateManager.cfg
	}()
	select {
	case <-readerDone:
		t.Fatal("control reader entered while composite epoch was partially applied")
	default:
	}
	close(cgroupManager.releaseUpdate)
	outcome := <-reloadDone
	if outcome.Err != nil || !outcome.Published || !outcome.Processed {
		t.Fatalf("OnConfigCandidate() outcome = %+v", outcome)
	}
	if observed := <-readerDone; observed != requested {
		t.Fatalf("reader observed config %p, want published candidate %p", observed, requested)
	}
	if stateManager.published != 1 || cgroupManager.cfg != requested || metricsCollector.cfg != requested {
		t.Fatal("components and CPU Points policy did not converge to one epoch")
	}
}

func TestOnConfigChangeRejectsRestartFieldsAndAppliesRuntimeFields(t *testing.T) {
	current := config.DefaultConfig()
	current.EnablePrometheus = true
	current.PrometheusMetricsBindHost = "127.0.0.1"
	current.PrometheusMetricsBindPort = 1974
	current.CgroupRoot = "/sys/fs/cgroup"
	current.CgroupBase = "resman"
	current.LogFile = "/var/log/resman.log"
	current.LogMaxSize = 10 * 1024 * 1024
	current.UseSyslog = false
	current.MCPEnabled = true
	current.MCPTransport = "http"
	current.MCPHTTPHost = "127.0.0.1"
	current.MCPHTTPPort = 1969
	current.MCPLogLevel = "INFO"
	current.MCPAuthToken = "current-token"
	current.MCPAllowWriteOps = false
	current.MCPShutdownTimeout = 10

	requested := config.DefaultConfig()
	requested.CPUThreshold = 88
	requested.LogLevel = "DEBUG"
	requested.EnablePrometheus = false
	requested.PrometheusMetricsBindHost = "0.0.0.0"
	requested.PrometheusMetricsBindPort = 9101
	requested.PrometheusTLSEnabled = true
	requested.CgroupRoot = "/other/cgroup"
	requested.CgroupBase = "other"
	requested.LogFile = "/var/log/resman-debug.log"
	requested.LogMaxSize = 20 * 1024 * 1024
	requested.UseSyslog = true
	requested.MCPEnabled = false
	requested.MCPTransport = "stdio"
	requested.MCPHTTPHost = "0.0.0.0"
	requested.MCPHTTPPort = 8080
	requested.MCPLogLevel = "DEBUG"
	requested.MCPAuthToken = "rotated-token"
	requested.MCPAllowWriteOps = true
	requested.MCPShutdownTimeout = 17

	stateManager := &testStateConfigManager{cfg: current}
	cgroupManager := &testCgroupConfigManager{}
	metricsCollector := &testMetricsConfigCollector{}
	var hookConfig *config.Config
	reloader := NewReloader(
		stateManager,
		cgroupManager,
		metricsCollector,
		func(cfg *config.Config) error {
			hookConfig = cfg
			return nil
		},
	)

	err := reloader.OnConfigChange(requested)
	var restartErr *config.RestartRequiredError
	if !errors.As(err, &restartErr) {
		t.Fatalf("OnConfigChange() error = %v, want RestartRequiredError", err)
	}

	if requested.CPUThreshold != 88 {
		t.Fatalf("CPUThreshold = %d, want 88", requested.CPUThreshold)
	}
	if requested.LogLevel != "DEBUG" {
		t.Fatalf("LogLevel = %q, want DEBUG", requested.LogLevel)
	}
	if requested.EnablePrometheus != current.EnablePrometheus ||
		requested.PrometheusMetricsBindHost != current.PrometheusMetricsBindHost ||
		requested.PrometheusMetricsBindPort != current.PrometheusMetricsBindPort ||
		requested.PrometheusTLSEnabled != current.PrometheusTLSEnabled {
		t.Fatal("Prometheus restart-required fields were applied at runtime")
	}
	if requested.CgroupRoot != current.CgroupRoot || requested.CgroupBase != current.CgroupBase {
		t.Fatal("cgroup restart-required fields were applied at runtime")
	}
	if requested.LogFile != current.LogFile ||
		requested.LogMaxSize != current.LogMaxSize ||
		requested.UseSyslog != current.UseSyslog {
		t.Fatal("logging restart-required fields were applied at runtime")
	}
	if requested.MCPEnabled != current.MCPEnabled ||
		requested.MCPTransport != current.MCPTransport ||
		requested.MCPHTTPHost != current.MCPHTTPHost ||
		requested.MCPHTTPPort != current.MCPHTTPPort ||
		requested.MCPLogLevel != current.MCPLogLevel ||
		requested.MCPAuthToken != current.MCPAuthToken ||
		requested.MCPAllowWriteOps != current.MCPAllowWriteOps ||
		requested.MCPShutdownTimeout != current.MCPShutdownTimeout {
		t.Fatal("MCP restart-required fields were applied at runtime")
	}
	if stateManager.cfg != requested || cgroupManager.cfg != requested ||
		metricsCollector.cfg != requested || hookConfig != requested {
		t.Fatal("components did not receive the same effective config")
	}
}

func TestOnConfigChangeBlocksCyclesUntilEveryConsumerHasOneEpoch(t *testing.T) {
	current := config.DefaultConfig()
	requested := config.DefaultConfig()
	requested.ProcessExcludeList = []string{"new-policy"}

	stateManager := &testStateConfigManager{cfg: current}
	cgroupManager := &testCgroupConfigManager{
		updateEntered: make(chan struct{}),
		releaseUpdate: make(chan struct{}),
	}
	metricsCollector := &testMetricsConfigCollector{cfg: current}
	reloader := NewReloader(stateManager, cgroupManager, metricsCollector)

	reloadDone := make(chan error)
	go func() {
		reloadDone <- reloader.OnConfigChange(requested)
	}()
	<-cgroupManager.updateEntered

	type observedEpoch struct {
		state   *config.Config
		cgroup  *config.Config
		metrics *config.Config
	}
	cycleDone := make(chan observedEpoch)
	go func() {
		leaveEpoch := stateManager.epoch.Enter()
		defer leaveEpoch()
		cycleDone <- observedEpoch{
			state:   stateManager.cfg,
			cgroup:  cgroupManager.cfg,
			metrics: metricsCollector.cfg,
		}
	}()

	select {
	case epoch := <-cycleDone:
		t.Fatalf("cycle entered during a partial update: %+v", epoch)
	default:
	}
	close(cgroupManager.releaseUpdate)
	if err := <-reloadDone; err != nil {
		t.Fatalf("OnConfigChange() error: %v", err)
	}
	epoch := <-cycleDone
	if epoch.state != requested || epoch.cgroup != requested || epoch.metrics != requested {
		t.Fatalf("cycle observed mixed epoch: %+v", epoch)
	}
}

func TestOnConfigChangeContinuesAfterComponentError(t *testing.T) {
	current := config.DefaultConfig()
	requested := config.DefaultConfig()
	requested.CPUThreshold = 88

	stateManager := &testStateConfigManager{cfg: current}
	cgroupManager := &testCgroupConfigManager{err: errors.New("update failed")}
	metricsCollector := &testMetricsConfigCollector{}
	hookCalled := false
	reloader := NewReloader(
		stateManager,
		cgroupManager,
		metricsCollector,
		func(*config.Config) error {
			hookCalled = true
			return nil
		},
	)

	if err := reloader.OnConfigChange(requested); err == nil {
		t.Fatal("OnConfigChange() should report the cgroup update error")
	}
	if stateManager.cfg != requested || metricsCollector.cfg != requested || !hookCalled {
		t.Fatal("component error aborted propagation to later components")
	}
}
