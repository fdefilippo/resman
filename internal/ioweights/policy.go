package ioweights

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fdefilippo/resman/internal/cpupoints"
)

const (
	// PolicyMapMarker identifies the only supported weighted-I/O map schema.
	PolicyMapMarker = "[resman-io-weights-map-v1]"
	// MaximumPolicyMapBytes bounds memory use and validation time.
	MaximumPolicyMapBytes = 64 * 1024
	// MaximumPolicyMapEntries bounds NSS work for one candidate snapshot.
	MaximumPolicyMapEntries = 4096
)

// Weight is the injective public weighted-I/O domain.
type Weight uint16

// NewWeight validates one public weighted-I/O value.
func NewWeight(value uint64) (Weight, error) {
	if value < 1 || value > 1000 {
		return 0, fmt.Errorf("weighted I/O value %d is outside 1..1000", value)
	}
	return Weight(value), nil
}

// Value returns the public integer representation.
func (w Weight) Value() uint64 { return uint64(w) }

// PolicyMapPath is an absolute clean path to a weighted-I/O user map.
type PolicyMapPath struct{ value string }

// NewPolicyMapPath validates a weighted-I/O map path.
func NewPolicyMapPath(path string) (PolicyMapPath, error) {
	if path == "" {
		return PolicyMapPath{}, fmt.Errorf("weighted I/O map path is empty")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return PolicyMapPath{}, fmt.Errorf("weighted I/O map path %q must be absolute and clean", path)
	}
	return PolicyMapPath{value: path}, nil
}

// String returns the validated path.
func (p PolicyMapPath) String() string { return p.value }

// PolicySource identifies the exact file object used for one policy snapshot.
type PolicySource struct {
	path   PolicyMapPath
	dev    uint64
	inode  uint64
	size   int64
	digest [sha256.Size]byte
}

// Path returns the validated source path.
func (s PolicySource) Path() PolicyMapPath { return s.path }

// Device returns the source device number.
func (s PolicySource) Device() uint64 { return s.dev }

// Inode returns the source inode.
func (s PolicySource) Inode() uint64 { return s.inode }

// Size returns the verified byte size.
func (s PolicySource) Size() int64 { return s.size }

// Digest returns the verified content digest.
func (s PolicySource) Digest() [sha256.Size]byte { return s.digest }

// UserWeight is one exact NSS-resolved map entry.
type UserWeight struct {
	username string
	uid      int
	weight   Weight
	line     int
}

// Username returns the exact mapped username.
func (w UserWeight) Username() string { return w.username }

// UID returns the resolved numeric identity.
func (w UserWeight) UID() int { return w.uid }

// Weight returns the configured weight.
func (w UserWeight) Weight() Weight { return w.weight }

// SourceLine returns the one-based map line.
func (w UserWeight) SourceLine() int { return w.line }

// PolicySnapshot is immutable after construction.
type PolicySnapshot struct {
	root      Weight
	defaultIO Weight
	entries   []UserWeight
	byUID     map[int]UserWeight
	source    PolicySource
}

// NewEmptyPolicySnapshot constructs a policy without mapped users.
func NewEmptyPolicySnapshot(root, defaultIO Weight) PolicySnapshot {
	return PolicySnapshot{root: root, defaultIO: defaultIO, byUID: make(map[int]UserWeight)}
}

// Root returns the root-session slice weight.
func (s PolicySnapshot) Root() Weight { return s.root }

// Default returns the weight for every non-mapped or ineligible slice.
func (s PolicySnapshot) Default() Weight { return s.defaultIO }

// Entries returns a defensive source-ordered copy.
func (s PolicySnapshot) Entries() []UserWeight { return append([]UserWeight(nil), s.entries...) }

// Source returns the verified map provenance.
func (s PolicySnapshot) Source() PolicySource { return s.source }

// WeightForUID returns a mapped eligible-user weight.
func (s PolicySnapshot) WeightForUID(uid int) (UserWeight, bool) {
	entry, ok := s.byUID[uid]
	return entry, ok
}

// ActiveUserSlice is one active sibling in user.slice.
type ActiveUserSlice struct {
	UID      int
	Eligible bool
}

// SliceClass identifies the policy source for one planned weight.
type SliceClass string

const (
	SliceClassRoot    SliceClass = "root"
	SliceClassMapped  SliceClass = "mapped"
	SliceClassDefault SliceClass = "default"
)

// SlicePlan is one immutable per-slice weighted-I/O decision.
type SlicePlan struct {
	uid    int
	class  SliceClass
	weight Weight
}

// UID returns the planned user identity.
func (p SlicePlan) UID() int { return p.uid }

// Class returns the source of the selected weight.
func (p SlicePlan) Class() SliceClass { return p.class }

// Weight returns the selected public-domain weight.
func (p SlicePlan) Weight() Weight { return p.weight }

// Plan builds a complete UID-sorted sibling plan. Ineligible and unmapped
// non-root slices deliberately receive the per-slice default.
func Plan(policy PolicySnapshot, active []ActiveUserSlice) ([]SlicePlan, error) {
	seen := make(map[int]struct{}, len(active))
	result := make([]SlicePlan, 0, len(active))
	for _, participant := range active {
		if participant.UID < 0 {
			return nil, fmt.Errorf("weighted I/O participant UID %d is invalid", participant.UID)
		}
		if _, duplicate := seen[participant.UID]; duplicate {
			return nil, fmt.Errorf("weighted I/O participant UID %d is duplicated", participant.UID)
		}
		seen[participant.UID] = struct{}{}
		planned := SlicePlan{uid: participant.UID, class: SliceClassDefault, weight: policy.defaultIO}
		if participant.UID == 0 {
			planned.class, planned.weight = SliceClassRoot, policy.root
		} else if participant.Eligible {
			if mapped, ok := policy.byUID[participant.UID]; ok {
				planned.class, planned.weight = SliceClassMapped, mapped.weight
			}
		}
		result = append(result, planned)
	}
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].uid < result[j-1].uid; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result, nil
}

type rawPolicyEntry struct {
	username string
	weight   Weight
	line     int
}

func parsePolicyMap(data []byte) ([]rawPolicyEntry, error) {
	if len(data) > MaximumPolicyMapBytes {
		return nil, fmt.Errorf("weighted I/O map is %d bytes; maximum is %d", len(data), MaximumPolicyMapBytes)
	}
	if bytes.Contains(data, []byte{0xef, 0xbb, 0xbf}) || !utf8.Valid(data) {
		return nil, fmt.Errorf("weighted I/O map must be valid UTF-8 without a BOM")
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.ContainsRune(content, '\r') {
		return nil, fmt.Errorf("weighted I/O map contains a carriage return outside a CRLF ending")
	}
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || lines[0] != PolicyMapMarker {
		return nil, fmt.Errorf("weighted I/O map line 1 must be exactly %s", PolicyMapMarker)
	}
	entries := make([]rawPolicyEntry, 0)
	seen := make(map[string]int)
	for index, line := range lines[1:] {
		lineNumber := index + 2
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.TrimSpace(line) != line {
			return nil, fmt.Errorf("weighted I/O map line %d has leading or trailing whitespace", lineNumber)
		}
		username, rawWeight, found := strings.Cut(line, "=")
		if !found || username == "" || rawWeight == "" || strings.Contains(rawWeight, "=") {
			return nil, fmt.Errorf("weighted I/O map line %d must be username=weight", lineNumber)
		}
		if prior, duplicate := seen[username]; duplicate {
			return nil, fmt.Errorf("weighted I/O map username %q is duplicated at lines %d and %d", username, prior, lineNumber)
		}
		value, err := strconv.ParseUint(rawWeight, 10, 16)
		if err != nil || strconv.FormatUint(value, 10) != rawWeight {
			return nil, fmt.Errorf("weighted I/O map line %d has non-canonical weight %q", lineNumber, rawWeight)
		}
		weight, err := NewWeight(value)
		if err != nil {
			return nil, fmt.Errorf("weighted I/O map line %d for username %q: %w", lineNumber, username, err)
		}
		if len(entries) == MaximumPolicyMapEntries {
			return nil, fmt.Errorf("weighted I/O map contains more than %d entries", MaximumPolicyMapEntries)
		}
		seen[username] = lineNumber
		entries = append(entries, rawPolicyEntry{username: username, weight: weight, line: lineNumber})
	}
	return entries, nil
}

// ExactIdentityResolver uses the established exact NSS result contract.
type ExactIdentityResolver = cpupoints.ExactIdentityResolver

// NSSIdentityResolver is the production CGO-aware exact NSS resolver.
type NSSIdentityResolver = cpupoints.NSSIdentityResolver
