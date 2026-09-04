package cpupoints

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// PolicyMapMarker is the required first line of every version-one policy map.
	// The complete grammar is UTF-8 without a BOM, LF or CRLF line endings, this
	// exact marker as the first physical line, then zero or more empty lines,
	// unindented full-line comments beginning with '#', or username=points
	// assignments. Assignments contain exactly one equals sign and no leading or
	// trailing whitespace. Usernames are passed to NSS byte-for-byte without
	// escaping or normalization; points are unsigned canonical decimal 1..1000.
	// A final line ending is optional.
	PolicyMapMarker = "[resman-cpu-points-map-v1]"
	// MaximumPolicyMapBytes bounds the complete policy file before parsing.
	MaximumPolicyMapBytes = 64 * 1024
	// MaximumPolicyMapEntries is the largest representable version-one map: 998
	// one-point guarantees plus the mandatory one-point root and best-effort
	// entitlements.
	MaximumPolicyMapEntries = 998
)

// PolicyMapPath is an absolute, clean path to the separate CPU Points map.
type PolicyMapPath struct{ value string }

// NewPolicyMapPath validates the internal map path without registering a public configuration key.
func NewPolicyMapPath(path string) (PolicyMapPath, error) {
	if path == "" {
		return PolicyMapPath{}, fmt.Errorf("CPU Points map path is empty")
	}
	if !utf8.ValidString(path) || containsControlRune(path) {
		return PolicyMapPath{}, fmt.Errorf("CPU Points map path contains invalid UTF-8 or control characters")
	}
	if strings.TrimSpace(path) != path {
		return PolicyMapPath{}, fmt.Errorf("CPU Points map path %q has leading or trailing whitespace", path)
	}
	if !filepath.IsAbs(path) {
		return PolicyMapPath{}, fmt.Errorf("CPU Points map path %q is not absolute", path)
	}
	if clean := filepath.Clean(path); clean != path {
		return PolicyMapPath{}, fmt.Errorf("CPU Points map path %q is not clean; use %q", path, clean)
	}
	if path == string(filepath.Separator) {
		return PolicyMapPath{}, fmt.Errorf("CPU Points map path must name a file, not the filesystem root")
	}
	return PolicyMapPath{value: path}, nil
}

// String returns the validated absolute map path.
func (p PolicyMapPath) String() string { return p.value }

// PolicyInputs are the complete typed inputs used to build one policy epoch.
type PolicyInputs struct {
	Reserve    ReservePoints
	Root       RootPoints
	BestEffort BestEffortPoints
	MapPath    PolicyMapPath
}

// AllocationClass identifies the CPU Points class selected for a UID.
type AllocationClass string

const (
	// AllocationClassGuaranteed identifies a UID explicitly present in the map.
	AllocationClassGuaranteed AllocationClass = "guaranteed"
	// AllocationClassBestEffort identifies an eligible UID absent from the map.
	AllocationClassBestEffort AllocationClass = "best_effort"
)

// ResolvedUserIdentity is one exact NSS result.
type ResolvedUserIdentity struct {
	Username string
	UID      int
}

// ExactIdentityResolver resolves a complete username without normalization.
// Returning zero or multiple identities rejects the complete snapshot.
type ExactIdentityResolver interface {
	ResolveExactUsername(username string) ([]ResolvedUserIdentity, error)
}

// NSSIdentityResolver resolves exact usernames through the CGO-aware os/user boundary.
type NSSIdentityResolver struct{}

// ResolveExactUsername resolves one username through NSS and rejects unrepresentable UIDs.
func (NSSIdentityResolver) ResolveExactUsername(username string) ([]ResolvedUserIdentity, error) {
	resolved, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("lookup username %q through NSS: %w", username, err)
	}
	uid, err := strconv.ParseUint(resolved.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("NSS returned unrepresentable UID %q for username %q: %w", resolved.Uid, username, err)
	}
	maxInt := uint64(^uint(0) >> 1)
	if uid > maxInt {
		return nil, fmt.Errorf("NSS returned UID %d for username %q, which is not representable as int", uid, username)
	}
	return []ResolvedUserIdentity{{Username: resolved.Username, UID: int(uid)}}, nil
}

// UserGuarantee is one resolved map entry with source provenance.
type UserGuarantee struct {
	username string
	uid      int
	points   ConfiguredGuaranteePoints
	line     int
}

// Username returns the exact name supplied to and returned by NSS.
func (g UserGuarantee) Username() string { return g.username }

// UID returns the resolved numeric identity.
func (g UserGuarantee) UID() int { return g.uid }

// Points returns the configured proportional entitlement.
func (g UserGuarantee) Points() ConfiguredGuaranteePoints { return g.points }

// SourceLine returns the one-based line number in the map file.
func (g UserGuarantee) SourceLine() int { return g.line }

// PolicySource identifies the exact opened object from which a snapshot was built.
type PolicySource struct {
	path   PolicyMapPath
	dev    uint64
	inode  uint64
	size   int64
	digest [sha256.Size]byte
}

// Path returns the validated source path.
func (s PolicySource) Path() PolicyMapPath { return s.path }

// Device returns the opened file's device number.
func (s PolicySource) Device() uint64 { return s.dev }

// Inode returns the opened file's inode number.
func (s PolicySource) Inode() uint64 { return s.inode }

// Size returns the verified source size in bytes.
func (s PolicySource) Size() int64 { return s.size }

// Digest returns the content identity validated while the policy was loaded.
func (s PolicySource) Digest() [sha256.Size]byte { return s.digest }

// PolicySnapshot is one immutable, completely resolved CPU Points policy epoch.
type PolicySnapshot struct {
	reserve         ReservePoints
	pool            ParentPoolPoints
	root            RootPoints
	bestEffort      BestEffortPoints
	configuredTotal ConfiguredGuaranteeTotalPoints
	entries         []UserGuarantee
	guaranteesByUID map[int]UserGuarantee
	source          PolicySource
}

// NewEmptyPolicySnapshot constructs a validated policy without mapped users.
// It exists for dependency-injected consumers that do not load a policy file;
// production startup must use PolicyLoader so file provenance is retained.
func NewEmptyPolicySnapshot(reserve ReservePoints, root RootPoints, bestEffort BestEffortPoints) (PolicySnapshot, error) {
	pool := reserve.ParentPool()
	if root.Value()+bestEffort.Value() > pool.Value() {
		return PolicySnapshot{}, newPolicyOvercommitError(pool, reserve, 0, root, bestEffort)
	}
	total, err := NewConfiguredGuaranteeTotalPoints(0)
	if err != nil {
		return PolicySnapshot{}, err
	}
	return PolicySnapshot{
		reserve:         reserve,
		pool:            pool,
		root:            root,
		bestEffort:      bestEffort,
		configuredTotal: total,
		guaranteesByUID: make(map[int]UserGuarantee),
	}, nil
}

// Reserve returns the nominal capacity kept outside the ResMan CPU parent.
func (s PolicySnapshot) Reserve() ReservePoints { return s.reserve }

// Pool returns the reserve-derived nominal parent pool.
func (s PolicySnapshot) Pool() ParentPoolPoints { return s.pool }

// Root returns the entitlement of an active root user slice.
func (s PolicySnapshot) Root() RootPoints { return s.root }

// BestEffort returns the aggregate best-effort entitlement.
func (s PolicySnapshot) BestEffort() BestEffortPoints { return s.bestEffort }

// ConfiguredGuaranteeTotal returns the sum of all mapped guarantees.
func (s PolicySnapshot) ConfiguredGuaranteeTotal() ConfiguredGuaranteeTotalPoints {
	return s.configuredTotal
}

// Entries returns a defensive copy in source order.
func (s PolicySnapshot) Entries() []UserGuarantee {
	return append([]UserGuarantee(nil), s.entries...)
}

// Source returns the opened file provenance retained by this snapshot.
func (s PolicySnapshot) Source() PolicySource { return s.source }

// GuaranteeForUID returns the mapped guarantee for a resolved UID.
func (s PolicySnapshot) GuaranteeForUID(uid int) (UserGuarantee, bool) {
	guarantee, ok := s.guaranteesByUID[uid]
	return guarantee, ok
}

// ClassForUID classifies an otherwise CPU-eligible UID. Absence from the map is
// always best effort and never creates a default per-user guarantee.
func (s PolicySnapshot) ClassForUID(uid int) AllocationClass {
	if _, ok := s.guaranteesByUID[uid]; ok {
		return AllocationClassGuaranteed
	}
	return AllocationClassBestEffort
}

type rawPolicyEntry struct {
	username string
	points   ConfiguredGuaranteePoints
	line     int
}

func parsePolicyMap(data []byte) ([]rawPolicyEntry, error) {
	if len(data) > MaximumPolicyMapBytes {
		return nil, fmt.Errorf("CPU Points map is %d bytes; maximum is %d", len(data), MaximumPolicyMapBytes)
	}
	if bytes.Contains(data, []byte{0xef, 0xbb, 0xbf}) {
		return nil, fmt.Errorf("CPU Points map contains a UTF-8 BOM")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("CPU Points map is not valid UTF-8")
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.ContainsRune(content, '\r') {
		return nil, fmt.Errorf("CPU Points map contains a carriage return outside a CRLF line ending")
	}
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || lines[0] != PolicyMapMarker {
		return nil, fmt.Errorf("CPU Points map line 1 must be exactly %s", PolicyMapMarker)
	}

	entries := make([]rawPolicyEntry, 0)
	seenNames := make(map[string]int)
	for index, line := range lines[1:] {
		lineNumber := index + 2
		if line == "" {
			continue
		}
		if containsControlRune(line) {
			return nil, fmt.Errorf("CPU Points map line %d contains a control character", lineNumber)
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line == PolicyMapMarker {
			return nil, fmt.Errorf("CPU Points map marker is duplicated at line %d", lineNumber)
		}
		if strings.TrimSpace(line) != line {
			return nil, fmt.Errorf("CPU Points map line %d has leading or trailing whitespace", lineNumber)
		}
		username, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("CPU Points map line %d must be username=points", lineNumber)
		}
		if username == "" {
			return nil, fmt.Errorf("CPU Points map line %d has an empty username", lineNumber)
		}
		if strings.Contains(value, "=") {
			return nil, fmt.Errorf("CPU Points map line %d contains an extra equals sign", lineNumber)
		}
		if strings.TrimSpace(username) != username || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("CPU Points map line %d has padded username or points", lineNumber)
		}
		if previousLine, duplicate := seenNames[username]; duplicate {
			return nil, fmt.Errorf("CPU Points map username %q is duplicated at lines %d and %d", username, previousLine, lineNumber)
		}
		points, err := parseConfiguredGuarantee(value)
		if err != nil {
			return nil, fmt.Errorf("CPU Points map line %d for username %q: %w", lineNumber, username, err)
		}
		if len(entries) == MaximumPolicyMapEntries {
			return nil, fmt.Errorf("CPU Points map contains more than %d entries", MaximumPolicyMapEntries)
		}
		seenNames[username] = lineNumber
		entries = append(entries, rawPolicyEntry{username: username, points: points, line: lineNumber})
	}
	return entries, nil
}

func parseConfiguredGuarantee(value string) (ConfiguredGuaranteePoints, error) {
	if value == "" {
		return ConfiguredGuaranteePoints{}, fmt.Errorf("guarantee is empty")
	}
	if len(value) > 1 && value[0] == '0' {
		return ConfiguredGuaranteePoints{}, fmt.Errorf("guarantee %q has a leading zero", value)
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return ConfiguredGuaranteePoints{}, fmt.Errorf("guarantee %q is not unsigned canonical decimal", value)
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return ConfiguredGuaranteePoints{}, fmt.Errorf("parse guarantee %q: %w", value, err)
	}
	return NewConfiguredGuaranteePoints(parsed)
}

func containsControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
