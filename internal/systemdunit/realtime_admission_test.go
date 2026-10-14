package systemdunit

import (
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// realtimeAwareTransport adds the two read-only realtime inspections to the
// shared fake transport.
type realtimeAwareTransport struct {
	*fakeUnitTransport
	loaded       []string
	scheduled    []string
	inspectErr   error
	loadedQuery  []string
	inspectCalls int
}

func (t *realtimeAwareTransport) loadedServices(_ context.Context, names []string) ([]string, error) {
	t.inspectCalls++
	t.loadedQuery = append([]string(nil), names...)
	if t.inspectErr != nil {
		return nil, t.inspectErr
	}
	var result []string
	for _, name := range names {
		for _, loaded := range t.loaded {
			if name == loaded {
				result = append(result, name)
			}
		}
	}
	return result, nil
}

func (t *realtimeAwareTransport) realtimeScheduledServices(context.Context) ([]string, error) {
	t.inspectCalls++
	if t.inspectErr != nil {
		return nil, t.inspectErr
	}
	return t.scheduled, nil
}

func revocableKernel() *realtimeKernelFacts {
	return &realtimeKernelFacts{groupScheduling: realtimeGroupSchedulingEnabled}
}

func TestRealtimeAdmissionGuardProtectsBeforeAnyProbe(t *testing.T) {
	// vmgclalpr2095/2098: had runs SCHED_FIFO inside vcs.service. Enabling cpu
	// at the root before vcs.service starts would make its realtime request
	// fail with EPERM, so the probe must never run while it is protected.
	tests := []struct {
		name       string
		kernel     *realtimeKernelFacts
		configured []string
		loaded     []string
		scheduled  []string
		wantUnit   string
		wantSource string
	}{
		{name: "configured unit loaded", kernel: revocableKernel(), configured: []string{"vcs.service"}, loaded: []string{"vcs.service"},
			wantUnit: "vcs.service", wantSource: RealtimeProtectionConfigured},
		{name: "unreadable kernel configuration is protected", kernel: &realtimeKernelFacts{groupScheduling: realtimeGroupSchedulingUnreadable},
			configured: []string{"vcs.service"}, loaded: []string{"vcs.service"}, wantUnit: "vcs.service", wantSource: RealtimeProtectionConfigured},
		{name: "unit with realtime scheduling policy", kernel: revocableKernel(), scheduled: []string{"rtapp.service"},
			wantUnit: "rtapp.service", wantSource: RealtimeProtectionCPUSchedulingPolicy},
		{name: "realtime policy is protected even after consent", kernel: revocableKernel(), configured: []string{}, scheduled: []string{"rtapp.service"},
			wantUnit: "rtapp.service", wantSource: RealtimeProtectionCPUSchedulingPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &realtimeAwareTransport{fakeUnitTransport: newFakeUnitTransport(), loaded: tt.loaded, scheduled: tt.scheduled}
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, newMemoryLeaseJournalStore())

			err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{
				RealtimeProtectedUnits: tt.configured, realtimeKernel: tt.kernel,
			})

			protection, ok := RealtimeProtectionFromError(err)
			if !ok {
				t.Fatalf("requireStartupCapabilities() error = %v, want a realtime protection", err)
			}
			if protection.Unit != tt.wantUnit || protection.Source != tt.wantSource {
				t.Fatalf("protection = %+v, want unit %s from %s", protection, tt.wantUnit, tt.wantSource)
			}
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || adapterErr.Reason != ReasonRealtimeAdmissionProtected {
				t.Fatalf("error %v does not carry %s", err, ReasonRealtimeAdmissionProtected)
			}
			if len(transport.probeStarts) != 0 {
				t.Fatalf("a capability probe ran %d times while a realtime service was protected", len(transport.probeStarts))
			}
		})
	}
}

func TestRealtimeAdmissionGuardLetsEnforcementProceed(t *testing.T) {
	tests := []struct {
		name         string
		kernel       *realtimeKernelFacts
		configured   []string
		loaded       []string
		wantInspects bool
	}{
		{name: "consent with no realtime policy", kernel: revocableKernel(), configured: []string{}, wantInspects: true},
		{name: "protected unit not installed", kernel: revocableKernel(), configured: []string{"vcs.service"}, wantInspects: true},
		{name: "root already delegates cpu", kernel: &realtimeKernelFacts{groupScheduling: realtimeGroupSchedulingEnabled, rootDelegatesCPU: true},
			configured: []string{"vcs.service"}, loaded: []string{"vcs.service"}},
		{name: "kernel without realtime group scheduling", kernel: &realtimeKernelFacts{groupScheduling: realtimeGroupSchedulingDisabled},
			configured: []string{"vcs.service"}, loaded: []string{"vcs.service"}},
		{name: "realtime runtime unlimited", kernel: &realtimeKernelFacts{groupScheduling: realtimeGroupSchedulingEnabled, runtimeUnlimited: true},
			configured: []string{"vcs.service"}, loaded: []string{"vcs.service"}},
		{name: "kernel facts not collected", kernel: nil, configured: []string{"vcs.service"}, loaded: []string{"vcs.service"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &realtimeAwareTransport{fakeUnitTransport: newFakeUnitTransport(), loaded: tt.loaded}
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, newMemoryLeaseJournalStore())

			err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{
				RealtimeProtectedUnits: tt.configured, realtimeKernel: tt.kernel,
			})

			if err != nil {
				t.Fatalf("requireStartupCapabilities() error = %v, want the probe to proceed", err)
			}
			if len(transport.probeStarts) == 0 {
				t.Fatal("the capability probe did not run")
			}
			if (transport.inspectCalls > 0) != tt.wantInspects {
				t.Fatalf("realtime service inspections = %d, want inspections %t", transport.inspectCalls, tt.wantInspects)
			}
		})
	}
}

func TestRealtimeAdmissionInspectionFailureIsNotAProtection(t *testing.T) {
	transport := &realtimeAwareTransport{fakeUnitTransport: newFakeUnitTransport(), inspectErr: errors.New("connection reset")}
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, newMemoryLeaseJournalStore())
	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{
		RealtimeProtectedUnits: []string{"vcs.service"}, realtimeKernel: revocableKernel(),
	})
	if err == nil || IsRealtimeProtectionError(err) {
		t.Fatalf("inspection failure = %v, want a transport error that is not a realtime protection", err)
	}
	if len(transport.probeStarts) != 0 {
		t.Fatal("the probe ran although realtime services could not be inspected")
	}
}

func TestRealtimeProtectionBecomesATypedRefusalUnderAuto(t *testing.T) {
	transport := &realtimeAwareTransport{fakeUnitTransport: newFakeUnitTransport(), loaded: []string{"vcs.service"}}
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, newMemoryLeaseJournalStore())
	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{
		RealtimeProtectedUnits: []string{"vcs.service"}, realtimeKernel: revocableKernel(),
	})
	if err != nil || refusal == nil || refusal.Realtime == nil {
		t.Fatalf("resolveStartupCapabilities() = (%+v, %v), want a realtime refusal", refusal, err)
	}
	if refusal.Realtime.Unit != "vcs.service" {
		t.Fatalf("refusal protects %q, want vcs.service", refusal.Realtime.Unit)
	}
}

func TestReadRealtimeKernelFacts(t *testing.T) {
	const release = "4.18.0-553.el8_10.x86_64"
	tests := []struct {
		name           string
		bootConfig     *string
		procConfig     *string
		runtime        *string
		subtree        *string
		wantScheduling string
		wantUnlimited  bool
		wantRootCPU    bool
		wantRevocable  bool
	}{
		{name: "EL8 field host before vcs", bootConfig: text("CONFIG_X=y\nCONFIG_RT_GROUP_SCHED=y\n"), runtime: text("950000\n"), subtree: text("memory pids\n"),
			wantScheduling: realtimeGroupSchedulingEnabled, wantRevocable: true},
		{name: "option not set", bootConfig: text("CONFIG_X=y\n# CONFIG_RT_GROUP_SCHED is not set\n"), runtime: text("950000"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingDisabled},
		{name: "option absent from a complete configuration", bootConfig: text("CONFIG_X=y\nCONFIG_Y=m\n"), runtime: text("950000"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingDisabled},
		{name: "empty boot configuration proves nothing", bootConfig: text(""), runtime: text("950000"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingUnreadable, wantRevocable: true},
		{name: "proc config.gz fallback", procConfig: text("CONFIG_RT_GROUP_SCHED=y\n"), runtime: text("950000"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingEnabled, wantRevocable: true},
		{name: "no configuration anywhere", runtime: text("950000"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingUnreadable, wantRevocable: true},
		{name: "unlimited realtime runtime", bootConfig: text("CONFIG_RT_GROUP_SCHED=y\n"), runtime: text("-1\n"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingEnabled, wantUnlimited: true},
		{name: "unreadable runtime counts as limited", bootConfig: text("CONFIG_RT_GROUP_SCHED=y\n"), subtree: text("memory"),
			wantScheduling: realtimeGroupSchedulingEnabled, wantRevocable: true},
		{name: "root already delegates cpu", bootConfig: text("CONFIG_RT_GROUP_SCHED=y\n"), runtime: text("950000"), subtree: text("cpuset cpu io memory pids\n"),
			wantScheduling: realtimeGroupSchedulingEnabled, wantRootCPU: true},
		{name: "cpuset alone is not cpu", bootConfig: text("CONFIG_RT_GROUP_SCHED=y\n"), runtime: text("950000"), subtree: text("cpuset memory"),
			wantScheduling: realtimeGroupSchedulingEnabled, wantRevocable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			paths := realtimeKernelPaths{
				release:        filepath.Join(root, "osrelease"),
				bootConfigDir:  filepath.Join(root, "boot"),
				procConfig:     filepath.Join(root, "config.gz"),
				runtimeSysctl:  filepath.Join(root, "sched_rt_runtime_us"),
				subtreeControl: filepath.Join(root, "cgroup.subtree_control"),
			}
			writeTestFile(t, paths.release, release+"\n")
			if tt.bootConfig != nil {
				writeTestFile(t, filepath.Join(paths.bootConfigDir, "config-"+release), *tt.bootConfig)
			}
			if tt.procConfig != nil {
				writeGzipTestFile(t, paths.procConfig, *tt.procConfig)
			}
			if tt.runtime != nil {
				writeTestFile(t, paths.runtimeSysctl, *tt.runtime)
			}
			if tt.subtree != nil {
				writeTestFile(t, paths.subtreeControl, *tt.subtree)
			}

			facts := readRealtimeKernelFacts(paths)

			if facts.groupScheduling != tt.wantScheduling || facts.runtimeUnlimited != tt.wantUnlimited ||
				facts.rootDelegatesCPU != tt.wantRootCPU || facts.revocationPossible() != tt.wantRevocable {
				t.Fatalf("facts = %+v revocable=%t, want scheduling=%s unlimited=%t rootCPU=%t revocable=%t",
					facts, facts.revocationPossible(), tt.wantScheduling, tt.wantUnlimited, tt.wantRootCPU, tt.wantRevocable)
			}
		})
	}
}

func TestProductionConstructorsCollectRealtimeKernelFacts(t *testing.T) {
	requirements := withRealtimeKernelFacts(StartupRequirements{RealtimeProtectedUnits: []string{"vcs.service"}}, t.TempDir())
	if requirements.realtimeKernel == nil {
		t.Fatal("production requirements carry no kernel facts, so the guard would never be evaluated")
	}
	if len(requirements.RealtimeProtectedUnits) != 1 {
		t.Fatalf("protected units lost: %v", requirements.RealtimeProtectedUnits)
	}
}

func text(value string) *string { return &value }

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGzipTestFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
