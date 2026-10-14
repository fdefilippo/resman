package systemdunit

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Realtime admission protection (resman-318.4).
//
// On a kernel built with CONFIG_RT_GROUP_SCHED, enabling the cgroup v2 cpu
// controller at the root gives every child cgroup a task group whose realtime
// runtime is zero, and cgroup v2 offers no knob to raise it. While
// kernel.sched_rt_runtime_us is not -1, sched_setscheduler(SCHED_FIFO or
// SCHED_RR) from inside such a group fails with EPERM. While ResMan's CPU
// limits are applied the controller stays enabled at the root, so a service
// that starts or restarts then (Veritas' hastart restarts vcs.service) loses
// realtime scheduling without anyone deciding so. An idle boot does not: the
// startup probe restores user.slice before readiness, and the EL8 cold boots
// show systemd withdrawing cpu again (test/functional/early-boot). The guard
// refuses to enforce at all on such a host unless the operator consented by
// leaving no protected unit.

// Kernel realtime group scheduling states.
const (
	realtimeGroupSchedulingEnabled    = "enabled"
	realtimeGroupSchedulingDisabled   = "disabled"
	realtimeGroupSchedulingUnreadable = "unreadable"
)

// Sources that make a unit protected.
const (
	RealtimeProtectionConfigured          = "configured"
	RealtimeProtectionCPUSchedulingPolicy = "cpu_scheduling_policy"
)

// realtimeKernelFacts is what the kernel says about realtime admission before
// ResMan touches the cgroup tree.
type realtimeKernelFacts struct {
	groupScheduling  string
	runtimeUnlimited bool
	rootDelegatesCPU bool
}

// revocationPossible reports whether enabling the cpu controller now could
// deny realtime admission to a service started afterwards. Every unreadable
// input counts as possible, so a missing fact never disables the protection.
func (f realtimeKernelFacts) revocationPossible() bool {
	return !f.rootDelegatesCPU && f.groupScheduling != realtimeGroupSchedulingDisabled && !f.runtimeUnlimited
}

// RealtimeProtection names the service that kept ResMan from enabling the cpu
// controller, why it is protected, and what the kernel reported.
type RealtimeProtection struct {
	Unit                   string
	Source                 string
	KernelGroupScheduling  string
	RootCPUDelegationState string
}

// RealtimeProtectionError reports that enabling the cpu controller would deny
// realtime admission to a protected service.
type RealtimeProtectionError struct {
	Protection RealtimeProtection
}

func (e *RealtimeProtectionError) Error() string {
	return fmt.Sprintf("enabling the cpu controller would deny realtime scheduling to %s (protected by %s; kernel realtime group scheduling %s, root cpu delegation %s); empty REALTIME_PROTECTED_UNITS to accept that",
		e.Protection.Unit, e.Protection.Source, e.Protection.KernelGroupScheduling, e.Protection.RootCPUDelegationState)
}

// IsRealtimeProtectionError reports whether startup stopped to protect a
// realtime service.
func IsRealtimeProtectionError(err error) bool {
	_, ok := RealtimeProtectionFromError(err)
	return ok
}

// RealtimeProtectionFromError extracts the protected service from err.
func RealtimeProtectionFromError(err error) (RealtimeProtection, bool) {
	var protection *RealtimeProtectionError
	if errors.As(err, &protection) {
		return protection.Protection, true
	}
	return RealtimeProtection{}, false
}

// realtimeServiceTransport reads which services are loaded and which declare a
// realtime CPUSchedulingPolicy. Both reads change nothing.
type realtimeServiceTransport interface {
	loadedServices(context.Context, []string) ([]string, error)
	realtimeScheduledServices(context.Context) ([]string, error)
}

// checkRealtimeAdmission refuses to let the startup probe enable the cpu
// controller while a protected service could lose realtime admission.
func (a *Adapter) checkRealtimeAdmission(ctx context.Context, requirements StartupRequirements) error {
	if requirements.realtimeKernel == nil {
		return nil
	}
	facts := *requirements.realtimeKernel
	if !facts.revocationPossible() {
		return nil
	}
	transport, ok := a.transport.(realtimeServiceTransport)
	if !ok {
		return &AdapterError{
			Reason: ReasonRealtimeAdmissionProtected, Operation: "realtime_admission",
			Err: fmt.Errorf("systemd transport cannot inspect services that need realtime scheduling"),
		}
	}
	protect := func(unit, source string) error {
		return &AdapterError{
			Reason: ReasonRealtimeAdmissionProtected, Operation: "realtime_admission", Unit: unit,
			Err: &RealtimeProtectionError{Protection: RealtimeProtection{
				Unit: unit, Source: source,
				KernelGroupScheduling:  facts.groupScheduling,
				RootCPUDelegationState: "absent",
			}},
		}
	}
	if len(requirements.RealtimeProtectedUnits) > 0 {
		loaded, err := transport.loadedServices(ctx, requirements.RealtimeProtectedUnits)
		if err != nil {
			return classifyTransportError("realtime_admission", "", err)
		}
		if len(loaded) > 0 {
			return protect(loaded[0], RealtimeProtectionConfigured)
		}
	}
	scheduled, err := transport.realtimeScheduledServices(ctx)
	if err != nil {
		return classifyTransportError("realtime_admission", "", err)
	}
	if len(scheduled) > 0 {
		return protect(scheduled[0], RealtimeProtectionCPUSchedulingPolicy)
	}
	return nil
}

// withRealtimeKernelFacts records the kernel facts the guard decides on. The
// production constructors always call it, so the guard is evaluated wherever
// ResMan may enable the cpu controller.
func withRealtimeKernelFacts(requirements StartupRequirements, cgroupRoot string) StartupRequirements {
	facts := readRealtimeKernelFacts(realtimeKernelPaths{
		release:        "/proc/sys/kernel/osrelease",
		bootConfigDir:  "/boot",
		procConfig:     "/proc/config.gz",
		runtimeSysctl:  "/proc/sys/kernel/sched_rt_runtime_us",
		subtreeControl: filepath.Join(cgroupRoot, "cgroup.subtree_control"),
	})
	requirements.realtimeKernel = &facts
	return requirements
}

type realtimeKernelPaths struct {
	release        string
	bootConfigDir  string
	procConfig     string
	runtimeSysctl  string
	subtreeControl string
}

func readRealtimeKernelFacts(paths realtimeKernelPaths) realtimeKernelFacts {
	facts := realtimeKernelFacts{groupScheduling: readRealtimeGroupScheduling(paths)}
	if value, err := os.ReadFile(paths.runtimeSysctl); err == nil {
		runtime, parseErr := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
		facts.runtimeUnlimited = parseErr == nil && runtime < 0
	}
	if value, err := os.ReadFile(paths.subtreeControl); err == nil {
		for _, controller := range strings.Fields(string(value)) {
			if controller == "cpu" {
				facts.rootDelegatesCPU = true
			}
		}
	}
	return facts
}

func readRealtimeGroupScheduling(paths realtimeKernelPaths) string {
	if release, err := os.ReadFile(paths.release); err == nil {
		name := "config-" + strings.TrimSpace(string(release))
		if file, err := os.Open(filepath.Join(paths.bootConfigDir, name)); err == nil {
			defer func() { _ = file.Close() }()
			if state, ok := scanRealtimeGroupScheduling(file); ok {
				return state
			}
		}
	}
	if file, err := os.Open(paths.procConfig); err == nil {
		defer func() { _ = file.Close() }()
		if reader, err := gzip.NewReader(file); err == nil {
			defer func() { _ = reader.Close() }()
			if state, ok := scanRealtimeGroupScheduling(reader); ok {
				return state
			}
		}
	}
	return realtimeGroupSchedulingUnreadable
}

// scanRealtimeGroupScheduling reads a kernel configuration. A complete
// configuration without the option means the option is not set; a file that
// holds no configuration at all proves nothing.
func scanRealtimeGroupScheduling(reader io.Reader) (string, bool) {
	scanner := bufio.NewScanner(reader)
	sawConfiguration := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "CONFIG_RT_GROUP_SCHED=y":
			return realtimeGroupSchedulingEnabled, true
		case "# CONFIG_RT_GROUP_SCHED is not set":
			return realtimeGroupSchedulingDisabled, true
		}
		if strings.HasPrefix(line, "CONFIG_") {
			sawConfiguration = true
		}
	}
	if scanner.Err() != nil || !sawConfiguration {
		return "", false
	}
	return realtimeGroupSchedulingDisabled, true
}
