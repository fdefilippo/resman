package cpupoints

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// OnlineCPUListPath is the authoritative Linux list of online logical host CPUs.
const OnlineCPUListPath = "/sys/devices/system/cpu/online"

// CapacityUnavailableReason is a bounded classification for live-capacity failures.
type CapacityUnavailableReason string

const (
	// CapacityReadError means the authoritative source could not be read.
	CapacityReadError CapacityUnavailableReason = "read_error"
	// CapacityParseError means the authoritative source did not contain a trustworthy CPU list.
	CapacityParseError CapacityUnavailableReason = "parse_error"
	// CapacityConversionError means the live count could not be represented as a parent quota.
	CapacityConversionError CapacityUnavailableReason = "conversion_error"
)

// CapacitySourceError identifies a bounded failure from the authoritative CPU source.
type CapacitySourceError struct {
	reason CapacityUnavailableReason
	err    error
}

// Error returns the source failure with its bounded reason.
func (e *CapacitySourceError) Error() string {
	return fmt.Sprintf("live CPU capacity %s: %v", e.reason, e.err)
}

// Unwrap exposes the underlying read or parse error.
func (e *CapacitySourceError) Unwrap() error { return e.err }

// Reason returns the bounded failure category.
func (e *CapacitySourceError) Reason() CapacityUnavailableReason { return e.reason }

// OnlineCPUSource reads one authoritative live CPU denominator.
type OnlineCPUSource interface {
	OnlineCPUs() (OnlineCPUCount, error)
}

// OnlineCPUSourceFunc adapts a function for deterministic provider tests.
type OnlineCPUSourceFunc func() (OnlineCPUCount, error)

// OnlineCPUs calls f.
func (f OnlineCPUSourceFunc) OnlineCPUs() (OnlineCPUCount, error) { return f() }

// SysfsOnlineCPUSource reads Linux's authoritative online logical CPU list.
type SysfsOnlineCPUSource struct {
	readFile func(string) ([]byte, error)
}

// NewSysfsOnlineCPUSource constructs the production live-capacity source.
func NewSysfsOnlineCPUSource() SysfsOnlineCPUSource {
	return SysfsOnlineCPUSource{readFile: os.ReadFile}
}

// OnlineCPUs reads and strictly parses the authoritative sysfs CPU list.
func (s SysfsOnlineCPUSource) OnlineCPUs() (OnlineCPUCount, error) {
	readFile := s.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	data, err := readFile(OnlineCPUListPath)
	if err != nil {
		return OnlineCPUCount{}, &CapacitySourceError{
			reason: CapacityReadError,
			err:    fmt.Errorf("read %s: %w", OnlineCPUListPath, err),
		}
	}
	count, err := ParseOnlineCPUList(string(data))
	if err != nil {
		return OnlineCPUCount{}, &CapacitySourceError{
			reason: CapacityParseError,
			err:    fmt.Errorf("parse %s: %w", OnlineCPUListPath, err),
		}
	}
	return count, nil
}

// CapacityState is an immutable provider snapshot. LastVerified remains the
// exact denominator/quota pair last derived from a trustworthy live read when
// Available is false; it must not be relabelled with an unverified denominator.
type CapacityState struct {
	Available         bool
	LastVerified      ParentQuota
	UnavailableReason CapacityUnavailableReason
}

type versionedCapacityState struct {
	revision uint64
	state    CapacityState
}

// LiveCapacityProvider refreshes enforcement capacity without consulting
// observation caches, runtime.NumCPU, procfs, or fallback constants.
type LiveCapacityProvider struct {
	source   OnlineCPUSource
	revision atomic.Uint64
	state    atomic.Pointer[versionedCapacityState]
}

// NewLiveCapacityProvider requires a trustworthy initial live denominator.
func NewLiveCapacityProvider(source OnlineCPUSource, pool ParentPoolPoints) (*LiveCapacityProvider, error) {
	if source == nil {
		return nil, fmt.Errorf("live CPU capacity source is required")
	}
	plan, _, err := readCapacityPlan(source, pool)
	if err != nil {
		return nil, fmt.Errorf("initialize live CPU capacity: %w", err)
	}

	p := &LiveCapacityProvider{source: source}
	p.revision.Store(1)
	p.state.Store(&versionedCapacityState{
		revision: 1,
		state: CapacityState{
			Available:    true,
			LastVerified: plan,
		},
	})
	return p, nil
}

// Refresh retries the authoritative read on every reconciliation. A runtime
// failure publishes one bounded unavailable state while retaining the exact
// last verified plan; it never synthesizes a denominator.
func (p *LiveCapacityProvider) Refresh(pool ParentPoolPoints) (CapacityState, error) {
	revision := p.revision.Add(1)
	plan, reason, err := readCapacityPlan(p.source, pool)
	if err != nil {
		state := p.publish(revision, func(current CapacityState) CapacityState {
			current.Available = false
			current.UnavailableReason = reason
			return current
		})
		return state, err
	}

	state := p.publish(revision, func(CapacityState) CapacityState {
		return CapacityState{
			Available:    true,
			LastVerified: plan,
		}
	})
	return state, nil
}

// State returns the current immutable capacity snapshot.
func (p *LiveCapacityProvider) State() CapacityState {
	return p.state.Load().state
}

func (p *LiveCapacityProvider) publish(revision uint64, next func(CapacityState) CapacityState) CapacityState {
	for {
		current := p.state.Load()
		if current.revision >= revision {
			return current.state
		}
		candidate := &versionedCapacityState{
			revision: revision,
			state:    next(current.state),
		}
		if p.state.CompareAndSwap(current, candidate) {
			return candidate.state
		}
	}
}

func readCapacityPlan(source OnlineCPUSource, pool ParentPoolPoints) (ParentQuota, CapacityUnavailableReason, error) {
	onlineCPUs, err := source.OnlineCPUs()
	if err != nil {
		var sourceErr *CapacitySourceError
		if errors.As(err, &sourceErr) {
			return ParentQuota{}, sourceErr.reason, err
		}
		return ParentQuota{}, CapacityReadError, &CapacitySourceError{reason: CapacityReadError, err: err}
	}
	plan, err := PlanParentQuota(onlineCPUs, pool)
	if err != nil {
		return ParentQuota{}, CapacityConversionError, &CapacitySourceError{reason: CapacityConversionError, err: err}
	}
	return plan, "", nil
}

// ParseOnlineCPUList strictly parses a Linux online-CPU list without expanding ranges.
func ParseOnlineCPUList(value string) (OnlineCPUCount, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return OnlineCPUCount{}, fmt.Errorf("online CPU list is empty")
	}
	if strings.IndexFunc(trimmed, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return OnlineCPUCount{}, fmt.Errorf("online CPU list contains whitespace")
	}

	var total uint64
	var previousEnd uint64
	havePrevious := false
	for _, segment := range strings.Split(trimmed, ",") {
		if segment == "" {
			return OnlineCPUCount{}, fmt.Errorf("online CPU list contains an empty segment")
		}
		start, end, err := parseCPUSegment(segment)
		if err != nil {
			return OnlineCPUCount{}, err
		}
		if havePrevious && start <= previousEnd {
			return OnlineCPUCount{}, fmt.Errorf("online CPU segment %q overlaps or is not strictly ordered", segment)
		}
		width := end - start
		if width == math.MaxUint64 {
			return OnlineCPUCount{}, fmt.Errorf("online CPU segment %q count overflows", segment)
		}
		count := width + 1
		if total > math.MaxUint64-count {
			return OnlineCPUCount{}, fmt.Errorf("online CPU count overflows")
		}
		total += count
		previousEnd = end
		havePrevious = true
	}
	return NewOnlineCPUCount(total)
}

func parseCPUSegment(segment string) (uint64, uint64, error) {
	if strings.Count(segment, "-") > 1 {
		return 0, 0, fmt.Errorf("online CPU segment %q has multiple range separators", segment)
	}
	parts := strings.SplitN(segment, "-", 2)
	start, err := parseCanonicalCPU(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid online CPU segment %q: %w", segment, err)
	}
	if len(parts) == 1 {
		return start, start, nil
	}
	end, err := parseCanonicalCPU(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid online CPU segment %q: %w", segment, err)
	}
	if end < start {
		return 0, 0, fmt.Errorf("online CPU segment %q has a descending range", segment)
	}
	return start, end, nil
}

func parseCanonicalCPU(value string) (uint64, error) {
	if value == "" {
		return 0, fmt.Errorf("CPU number is empty")
	}
	if len(value) > 1 && value[0] == '0' {
		return 0, fmt.Errorf("CPU number %q has a leading zero", value)
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("CPU number %q is not unsigned decimal", value)
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse CPU number %q: %w", value, err)
	}
	return parsed, nil
}
