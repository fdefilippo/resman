//go:build linux && resman_native

package state

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

// TestNativeBlockIOPSDecision qualifies the production source, systemd adapter,
// decision collector and hard-I/O properties in an explicitly disposable VM.
func TestNativeBlockIOPSDecision(t *testing.T) {
	if os.Getenv("RESMAN_NATIVE_DISPOSABLE_GUEST") != "resman-82p" {
		t.Skip("disposable guest required")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
		return string(out)
	}
	run("systemd-run", "--quiet", "--unit=resman82p-user-keeper", "--slice=user-1000.slice", "--uid=1000", "/usr/bin/sleep", "300")
	defer exec.Command("systemctl", "stop", "resman82p-user-keeper.service").Run()
	run("mkdir", "-p", "/var/lib/resman")
	run("chmod", "0700", "/var/lib/resman")
	adapter, err := systemdunit.New(context.Background(), "/sys/fs/cgroup", 5*time.Second, systemdunit.StartupRequirements{IO: true})
	if err != nil {
		t.Fatal(err)
	}
	newConfig := func() *config.Config {
		cfg := config.DefaultConfig()
		cfg.IOEnabled = true
		cfg.IOReadBPS = "max"
		cfg.IOWriteBPS = "max"
		cfg.IOReadIOPS = 100
		cfg.IOWriteIOPS = 100
		cfg.IOThreshold = 50
		cfg.IOReleaseThreshold = 25
		cfg.IOThresholdDuration = 0
		cfg.CPUThresholdDuration = 0
		cfg.MinActiveTime = 0
		cfg.IgnoreSystemLoad = true
		cfg.IODeviceFilter = strings.TrimSpace(run("cat", "/sys/block/dm-0/dev"))
		cfg.ProcessExcludeList = nil
		return cfg
	}
	cfg := newConfig()
	manager, err := NewManager(cfg, &mockMetricsCollector{usernames: map[int]string{1000: "oracle"}}, &mockCgroupManager{}, nil, WithSystemdCPUEnforcement(adapter), WithBlockIOPSRuntime())
	if err != nil {
		adapter.Close()
		t.Fatal(err)
	}
	defer manager.Cleanup()
	collect := func() *SystemMetrics {
		t.Helper()
		topology, err := adapter.Discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inventory, err := adapter.CaptureProcessAuthorityInventory(context.Background(), topology, time.Now().UnixNano(), true)
		if err != nil {
			t.Fatal(err)
		}
		sample := &SystemMetrics{IOEligibleUsers: []int{1000}, IOEligibleUsersCount: 1, TotalCores: 2, TotalMemoryMB: 4096, UserMetrics: make(map[int]*resmanmetrics.UserMetrics), systemdAuthorityInventory: &inventory}
		if err := manager.collectEligibleBlockIOPS(sample, cfg.GetIODecisionPolicy()); err != nil {
			t.Fatal(err)
		}
		t.Logf("unavailable=%d bounds=%+v", sample.IOBlockIOPSUnavailableUsers, sample.IOBlockIOPSDevices)
		return sample
	}
	first := collect()
	if first.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatal("first interval must warm up")
	}
	run("rm", "-f", "/var/tmp/resman82p-state-data", "/var/tmp/resman82p-state-fio-result")
	run("/usr/bin/fio", "--name=prepare", "--filename=/var/tmp/resman82p-state-data", "--rw=write", "--bs=4k", "--size=256k", "--direct=1", "--ioengine=sync", "--output=/var/tmp/resman82p-prepare-result")
	run("chown", "1000", "/var/tmp/resman82p-state-data")
	run("chmod", "0666", "/var/tmp/resman82p-state-data")
	fio := func(direction string) {
		run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-user-fio", "--slice=user-1000.slice", "--uid=1000", "/usr/bin/fio", "--name=direct", "--filename=/var/tmp/resman82p-state-data", "--rw="+direction, "--bs=4k", "--size=256k", "--direct=1", "--ioengine=sync", "--output=/var/tmp/resman82p-state-fio-result")
	}
	// Establish the baseline immediately before pressure rather than including setup.
	collect()
	fio("write")
	busy := collect()
	decision, reason := manager.makeDecision(busy)
	t.Logf("write decision=%s reason=%s", decision, reason)
	if decision != decisionActivate {
		t.Fatal("IOPS-only traffic did not activate")
	}
	topology, _ := adapter.Discover(context.Background())
	var user systemdunit.UnitIdentity
	for _, u := range topology.Users {
		if u.UID == 1000 {
			user = u.Unit.Identity
		}
	}
	assignment, err := systemdunit.NewDevicePropertyAssignment(systemdunit.PropertyIOWriteIOPSMax, []systemdunit.DeviceLimit{{Path: "/dev/dm-0", Value: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), user, []systemdunit.PropertyAssignment{assignment}); err != nil {
		t.Fatal(err)
	}
	fio("write")
	during := collect()
	if during.IOBlockIOPSUnavailableUsers != 0 {
		t.Fatal("source lost coverage while limited")
	}
	minimum := 0.0
	for _, bounds := range during.IOBlockIOPSDevices {
		minimum += bounds.Write.ActivationMinimum
	}
	if minimum <= 0 {
		t.Fatal("source stopped counting during enforcement")
	}
	t.Logf("io.max=%s", run("cat", "/sys/fs/cgroup/user.slice/user-1000.slice/io.max"))
	readLimit, err := systemdunit.NewDevicePropertyAssignment(systemdunit.PropertyIOReadIOPSMax, []systemdunit.DeviceLimit{{Path: "/dev/dm-0", Value: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), user, []systemdunit.PropertyAssignment{readLimit}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.RestoreProperties(context.Background(), user, []systemdunit.PropertyName{systemdunit.PropertyIOWriteIOPSMax}); err != nil {
		t.Fatal(err)
	}
	fio("read")
	reads := collect()
	decision, reason = manager.makeDecision(reads)
	t.Logf("read decision=%s reason=%s", decision, reason)
	if decision != decisionActivate {
		t.Fatal("read IOPS did not activate")
	}
	time.Sleep(150 * time.Millisecond)
	collect()
	time.Sleep(250 * time.Millisecond)
	quiet := collect()
	quota, err := systemdunit.NewCPUQuotaAssignmentsFromCgroupMax(50000, 100000)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := systemdunit.NewPropertyAssignment(systemdunit.PropertyMemoryMax, 512*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), user, append(quota, memory)); err != nil {
		t.Fatal(err)
	}
	t.Logf("active CPU=%s RAM=%s", run("cat", "/sys/fs/cgroup/user.slice/user-1000.slice/cpu.max"), run("cat", "/sys/fs/cgroup/user.slice/user-1000.slice/memory.max"))
	manager.limitsActive = true
	manager.limitsAppliedTime = time.Now().Add(-time.Hour)
	if decision, reason := manager.makeDecision(quiet); decision != decisionDeactivate {
		t.Fatalf("CPU release pinned: %s: %s", decision, reason)
	}
	manager.limitsActive = false
	manager.resourceLimitsActive = true
	if decision, reason := manager.makeDecision(quiet); decision != decisionDeactivate {
		t.Fatalf("RAM release pinned: %s: %s", decision, reason)
	}
	// Releasing every owned property can disable I/O accounting. Its next interval
	// must warm up rather than certify a zero across an unobserved gap.
	if err := manager.deactivateLimits(); err != nil {
		t.Fatal(err)
	}
	t.Logf("restored properties=%s", run("systemctl", "show", "user-1000.slice", "-p", "CPUQuotaPerSecUSec", "-p", "MemoryMax", "-p", "IOAccounting"))
	if next := collect(); next.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatal("restoration gap inherited a baseline")
	}
	reloaded := newConfig()
	reloaded.ProcessExcludeList = []string{"^fio$"}
	finish, err := manager.PrepareBlockIOPSConfig(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	manager.UpdateConfig(reloaded)
	finish()
	if sample := collect(); sample.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatal("changed selection inherited an old baseline")
	}
	fio("write")
	excluded := collect()
	for _, b := range excluded.IOBlockIOPSDevices {
		if b.Write.ActivationMinimum != 0 {
			t.Fatal("excluded direct I/O entered the activation minimum")
		}
	}
	if decision, reason := manager.makeDecision(excluded); decision == decisionActivate {
		t.Fatalf("excluded I/O activated: %s", reason)
	}
	if _, err := adapter.Apply(context.Background(), user, append(quota, memory)); err != nil {
		t.Fatal(err)
	}
	manager.limitsActive = true
	manager.resourceLimitsActive = true
	if err := manager.blockIOPS.source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.collectEligibleBlockIOPS(excluded, reloaded.GetIODecisionPolicy()); err == nil {
		t.Fatal("lost native source did not fail collection")
	} else {
		t.Logf("source loss=%v", err)
	}
	if manager.limitsActive || manager.resourceLimitsActive {
		t.Fatal("source loss retained CPU/RAM episode")
	}
	t.Logf("properties after source loss=%s", run("systemctl", "show", "user-1000.slice", "-p", "CPUQuotaPerSecUSec", "-p", "MemoryMax", "-p", "IOAccounting"))
}
