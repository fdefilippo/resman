//go:build linux

package blockiops

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/internal/processpolicy"
	"golang.org/x/sys/unix"
)

//go:embed bpf/collector_bpfel.o
var collectorObject []byte

//go:generate ../../scripts/generate-block-iops.sh

var nextSourceGeneration atomic.Uint64

// KernelSource owns its programs, links, bounded maps and frozen process policy.
// It performs no process relocation or filesystem/cgroup mutation. Registration
// requires an independently confirmed systemd unit with I/O accounting enabled.
type KernelSource struct {
	gate       operationgate.Gate
	collection *ebpf.Collection
	links      []link.Link
	generation uint64
	policy     uint64
	selection  *config.Config
	subjects   map[uint64]kernelSubject
	nextCookie uint64
	failure    error
}

type kernelSubject struct {
	identity Identity
	cookie   uint64
}

// counterKey matches the checked-in CO-RE object's explicit layout.
type counterKey struct {
	Cgroup     uint64
	Lifetime   uint64
	Major      uint32
	Minor      uint32
	Direction  uint32
	Reserved   uint32
	Executable [256]byte
}

// NewKernelSource proves the mandatory BTF/attachment capability before returning
// a source. An unsupported or unqualified profile is an explicit startup error,
// never an always-unavailable decision producer.
func NewKernelSource(policyGeneration uint64, cfg *config.Config) (*KernelSource, error) {
	if cfg == nil || policyGeneration == 0 {
		return nil, fmt.Errorf("block-IOPS source requires an executable policy generation")
	}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return nil, fmt.Errorf("identify block-IOPS kernel: %w", err)
	}
	release := unix.ByteSliceToString(uts.Release[:])
	// Qualification is tied to actual executed distribution builds. BTF shape
	// alone cannot certify the semantic meaning of a private kernel function.
	switch release {
	case "5.14.0-687.66.1.el9_8.x86_64", "6.12.0-204.92.4.2.el9uek.x86_64":
	default:
		return nil, fmt.Errorf("block-IOPS kernel profile %q is not qualified; see docs/BLOCK-IOPS-SOURCE-DESIGN.md", release)
	}
	kernelTypes, err := btf.LoadKernelSpec()
	if err != nil {
		return nil, fmt.Errorf("block-IOPS requires readable kernel BTF: %w", err)
	}
	if err := checkKernelABI(kernelTypes); err != nil {
		return nil, err
	}
	patterns := cfg.GetProcessExcludeList()
	for _, pattern := range patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("block-IOPS executable exclusion %q: %w", pattern, err)
		}
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(collectorObject))
	if err != nil {
		return nil, fmt.Errorf("decode block-IOPS kernel object: %w", err)
	}
	collection, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		Programs: ebpf.ProgramOptions{KernelTypes: kernelTypes},
	})
	if err != nil {
		return nil, fmt.Errorf("load block-IOPS programs (BPF/perf privileges and supported kernel required): %w", err)
	}
	source := &KernelSource{
		collection: collection, generation: nextSourceGeneration.Add(1), policy: policyGeneration,
		selection: &config.Config{ProcessExcludeList: patterns}, subjects: make(map[uint64]kernelSubject),
	}
	// The accounting probe attaches last: it can never observe a partially
	// attached provenance path as a certified selected operation.
	for _, name := range []string{"finish_direct", "clone_direct", "capture_direct", "account_operation"} {
		attached, err := link.AttachTracing(link.TracingOptions{Program: collection.Programs[name]})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("attach block-IOPS %s: %w", name, err), source.Close())
		}
		source.links = append(source.links, attached)
	}
	if err := source.checkHealthLocked(); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return source, nil
}

func checkKernelABI(spec *btf.Spec) error {
	for _, expected := range []struct {
		name   string
		params []string
	}{
		{"blk_cgroup_bio_start", []string{"bio"}},
		{"iomap_dio_submit_bio", []string{"iter", "dio", "bio", "pos"}},
		{"bio_alloc_clone", []string{"bdev", "bio_src", "gfp", "bs"}},
	} {
		var function *btf.Func
		if err := spec.TypeByName(expected.name, &function); err != nil {
			return fmt.Errorf("block-IOPS kernel function %s unavailable: %w", expected.name, err)
		}
		prototype, ok := function.Type.(*btf.FuncProto)
		if !ok || len(prototype.Params) != len(expected.params) {
			return fmt.Errorf("block-IOPS kernel function %s has an unsupported prototype", expected.name)
		}
		for index, name := range expected.params {
			if prototype.Params[index].Name != name {
				return fmt.Errorf("block-IOPS kernel function %s argument %d is not %s", expected.name, index, name)
			}
		}
	}
	var info *btf.Struct
	if err := spec.TypeByName("bpf_prog_info", &info); err != nil {
		return fmt.Errorf("block-IOPS recursion-loss ABI unavailable: %w", err)
	}
	for _, member := range info.Members {
		if member.Name == "recursion_misses" {
			return nil
		}
	}
	return fmt.Errorf("block-IOPS recursion-loss counter unavailable")
}

// Register begins a watch lifetime after systemd identity and IOAccounting were
// verified. Re-registering the same lifetime preserves its cumulative counters.
func (s *KernelSource) Register(uid uint32, invocation string, cgroupID uint64) (Identity, error) {
	leave := s.gate.Enter()
	defer leave()
	if err := s.checkHealthLocked(); err != nil {
		return Identity{}, err
	}
	if invocation == "" || cgroupID == 0 {
		return Identity{}, fmt.Errorf("block-IOPS registration requires a confirmed unit lifetime")
	}
	id := Identity{SourceGeneration: s.generation, PolicyGeneration: s.policy, UID: uid, InvocationID: invocation, CgroupID: cgroupID}
	if previous, exists := s.subjects[cgroupID]; exists {
		id.WatchGeneration = previous.cookie
		if previous.identity == id {
			return id, nil
		}
	}
	if s.nextCookie == math.MaxUint64 {
		return Identity{}, s.failLocked(fmt.Errorf("block-IOPS watch lifetime overflow"))
	}
	s.nextCookie++
	id.WatchGeneration = s.nextCookie
	if err := s.collection.Maps["watched"].Update(cgroupID, s.nextCookie, ebpf.UpdateAny); err != nil {
		return Identity{}, s.failLocked(fmt.Errorf("register block-IOPS cgroup %d: %w", cgroupID, err))
	}
	s.subjects[cgroupID] = kernelSubject{identity: id, cookie: s.nextCookie}
	return id, nil
}

// Read returns monotonic cumulative partitions for exactly one registered unit.
// Configured devices with no counted operations have a measured zero because
// registration precedes this read and the healthy accounting program counts
// every read/write BIO at that unit's quota boundary.
func (s *KernelSource) Read(identity Identity, devices []Device) (Sample, error) {
	leave := s.gate.Enter()
	defer leave()
	if err := s.checkHealthLocked(); err != nil {
		return Sample{}, err
	}
	subject, exists := s.subjects[identity.CgroupID]
	if !exists || subject.identity != identity {
		return Sample{}, fmt.Errorf("block-IOPS watch lifetime is unavailable")
	}
	sample := Sample{Identity: identity, Started: time.Now(), Devices: make(map[Device]Counters, len(devices))}
	configuredDevices := make(map[[2]uint32]Device, len(devices))
	for _, device := range devices {
		sample.Devices[device] = Counters{}
		configuredDevices[[2]uint32{device.Major, device.Minor}] = device
	}
	var key counterKey
	var perCPU []uint64
	iterator := s.collection.Maps["operations"].Iterate()
	for iterator.Next(&key, &perCPU) {
		if key.Cgroup != identity.CgroupID || key.Lifetime != subject.cookie {
			continue
		}
		device, configured := configuredDevices[[2]uint32{key.Major, key.Minor}]
		counters := sample.Devices[device]
		if !configured {
			continue
		}
		if key.Reserved != 0 || key.Direction > 1 {
			return Sample{}, s.failLocked(fmt.Errorf("invalid block-IOPS kernel record"))
		}
		partition := &counters.Read
		if key.Direction == 1 {
			partition = &counters.Write
		}
		category := s.categoryCounter(partition, key.Executable[:])
		for _, count := range perCPU {
			if count > math.MaxUint64-*category {
				return Sample{}, s.failLocked(fmt.Errorf("block-IOPS aggregate counter overflow"))
			}
			*category += count
		}
		sample.Devices[device] = counters
	}
	if err := iterator.Err(); err != nil {
		return Sample{}, s.failLocked(fmt.Errorf("read block-IOPS counters: %w", err))
	}
	sample.Finished = time.Now()
	// This check runs after the read window, so any loss that could affect the
	// returned counters invalidates them. A failure is sticky for this source.
	if err := s.checkHealthLocked(); err != nil {
		return Sample{}, err
	}
	return sample, nil
}

func (s *KernelSource) categoryCounter(partition *Partition, rawName []byte) *uint64 {
	if len(s.selection.ProcessExcludeList) == 0 {
		return &partition.Selected
	}
	end := bytes.IndexByte(rawName, 0)
	if end <= 0 {
		return &partition.Unresolved
	}
	name := string(rawName[:end])
	// Procfs adds a deletion suffix and processpolicy normalizes whitespace.
	// Without proving that rendering for unusual names, keep them unresolved.
	if strings.TrimSpace(name) != name || strings.HasSuffix(name, " (deleted)") {
		return &partition.Unresolved
	}
	selected := processpolicy.Evaluate(s.selection, "/"+name, "")
	if !selected.IdentityTrusted {
		return &partition.Unresolved
	}
	if !selected.Enforceable {
		return &partition.Excluded
	}
	return &partition.Selected
}

func (s *KernelSource) checkHealthLocked() error {
	if s.failure != nil {
		return s.failure
	}
	if s.collection == nil {
		return fmt.Errorf("block-IOPS source is closed")
	}
	for index := uint32(1); index < 5; index++ {
		var failures uint64
		if err := s.collection.Maps["health"].Lookup(index, &failures); err != nil {
			return s.failLocked(fmt.Errorf("read block-IOPS integrity counter %d: %w", index, err))
		}
		if failures != 0 {
			return s.failLocked(fmt.Errorf("block-IOPS source integrity failure %d (%d events)", index, failures))
		}
	}
	for name, program := range s.collection.Programs {
		stats, err := program.Stats()
		if err != nil {
			return s.failLocked(fmt.Errorf("inspect block-IOPS %s recursion loss: %w", name, err))
		}
		if stats.RecursionMisses != 0 {
			return s.failLocked(fmt.Errorf("block-IOPS %s missed %d recursive executions", name, stats.RecursionMisses))
		}
	}
	for _, attached := range s.links {
		info, err := attached.Info()
		if err != nil {
			return s.failLocked(fmt.Errorf("inspect block-IOPS attachment: %w", err))
		}
		if info.Program == 0 {
			return s.failLocked(fmt.Errorf("block-IOPS program detached"))
		}
	}
	return nil
}

func (s *KernelSource) failLocked(err error) error {
	s.failure = err
	return err
}

// Close serializes against all reads and registrations, detaches accounting
// before provenance, and releases every descriptor owned by this source.
func (s *KernelSource) Close() error {
	leave := s.gate.Enter()
	defer leave()
	var result error
	for index := len(s.links) - 1; index >= 0; index-- {
		if err := s.links[index].Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("detach block-IOPS program: %w", err))
		}
	}
	s.links = nil
	if s.collection != nil {
		s.collection.Close()
		s.collection = nil
	}
	return result
}

// Health verifies the source even when there are no registered eligible users.
func (s *KernelSource) Health() error {
	leave := s.gate.Enter()
	defer leave()
	return s.checkHealthLocked()
}

// Retain removes watches for retired or no-longer-authoritative unit lifetimes.
// Cumulative rows are never deleted during a live reader lifetime; bounded map
// exhaustion is an integrity failure requiring a new source, never eviction.
func (s *KernelSource) Retain(identities []Identity) error {
	leave := s.gate.Enter()
	defer leave()
	if err := s.checkHealthLocked(); err != nil {
		return err
	}
	keep := make(map[Identity]bool, len(identities))
	for _, id := range identities {
		keep[id] = true
	}
	for cgroup, subject := range s.subjects {
		if keep[subject.identity] {
			continue
		}
		if err := s.collection.Maps["watched"].Delete(cgroup); err != nil {
			return s.failLocked(fmt.Errorf("retire IOPS watch: %w", err))
		}
		delete(s.subjects, cgroup)
	}
	return nil
}
