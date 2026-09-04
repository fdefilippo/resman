package systemdunit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	leaseJournalVersion  = 1
	maxLeaseJournalBytes = 1 << 20

	// DefaultLeaseJournalPath is the durable ownership record for systemd runtime properties.
	DefaultLeaseJournalPath = "/var/lib/resman/systemd-property-leases.json"
)

type leasePhase string

const (
	leasePhaseApplying  leasePhase = "applying"
	leasePhaseApplied   leasePhase = "applied"
	leasePhaseRestoring leasePhase = "restoring"
	leasePhaseReloading leasePhase = "reloading"
)

type durableLeaseJournal struct {
	Version    int                `json:"version"`
	Generation uint64             `json:"generation"`
	Units      []durableUnitLease `json:"units"`
}

type durableUnitLease struct {
	Unit              string                   `json:"unit"`
	Identity          durableUnitIdentity      `json:"identity"`
	Phase             leasePhase               `json:"phase"`
	Properties        []durablePropertyLease   `json:"properties"`
	Footprint         []durableFileFingerprint `json:"footprint"`
	PreviousFootprint []durableFileFingerprint `json:"previous_footprint,omitempty"`
}

type durableUnitIdentity struct {
	ObjectPath     string `json:"object_path"`
	InvocationID   string `json:"invocation_id"`
	ControlGroupID uint64 `json:"control_group_id"`
}

type durablePropertyLease struct {
	Property        PropertyName `json:"property"`
	Baseline        uint64       `json:"baseline"`
	PreviousApplied uint64       `json:"previous_applied"`
	LastApplied     uint64       `json:"last_applied"`
	Uncertain       bool         `json:"uncertain"`
	NewLease        bool         `json:"new_lease"`
}

type durableFileFingerprint struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
}

type leaseJournalStore interface {
	Load() (durableLeaseJournal, error)
	Save(durableLeaseJournal) error
}

type fileLeaseJournalStore struct {
	path     string
	ownerUID int
}

func newFileLeaseJournalStore(path string) *fileLeaseJournalStore {
	return newFileLeaseJournalStoreForOwner(path, os.Geteuid())
}

func newFileLeaseJournalStoreForOwner(path string, ownerUID int) *fileLeaseJournalStore {
	return &fileLeaseJournalStore{path: path, ownerUID: ownerUID}
}

func (s *fileLeaseJournalStore) Load() (durableLeaseJournal, error) {
	if err := ensurePrivateLeaseDirectory(filepath.Dir(s.path), s.ownerUID); err != nil {
		return durableLeaseJournal{}, err
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return durableLeaseJournal{Version: leaseJournalVersion}, nil
	}
	if err != nil {
		return durableLeaseJournal{}, fmt.Errorf("inspect systemd property lease journal %s: %w", s.path, err)
	}
	if err := validateLeaseJournalInfo(s.path, info, s.ownerUID); err != nil {
		return durableLeaseJournal{}, err
	}
	file, err := os.OpenFile(s.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return durableLeaseJournal{}, fmt.Errorf("open systemd property lease journal %s: %w", s.path, err)
	}
	openedBefore, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return durableLeaseJournal{}, fmt.Errorf("inspect opened systemd property lease journal %s: %w", s.path, err)
	}
	if err := validateLeaseJournalInfo(s.path, openedBefore, s.ownerUID); err != nil {
		_ = file.Close()
		return durableLeaseJournal{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxLeaseJournalBytes+1))
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return durableLeaseJournal{}, fmt.Errorf("read systemd property lease journal %s: %w", s.path, readErr)
	}
	if statErr != nil {
		return durableLeaseJournal{}, fmt.Errorf("inspect opened systemd property lease journal %s: %w", s.path, statErr)
	}
	if closeErr != nil {
		return durableLeaseJournal{}, fmt.Errorf("close systemd property lease journal %s: %w", s.path, closeErr)
	}
	if len(data) > maxLeaseJournalBytes {
		return durableLeaseJournal{}, fmt.Errorf("systemd property lease journal %s exceeds %d bytes", s.path, maxLeaseJournalBytes)
	}
	pathInfo, err := os.Lstat(s.path)
	if err != nil {
		return durableLeaseJournal{}, fmt.Errorf("reinspect systemd property lease journal %s: %w", s.path, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedBefore) || !os.SameFile(openedInfo, pathInfo) || openedBefore.Size() != openedInfo.Size() || !openedBefore.ModTime().Equal(openedInfo.ModTime()) || int64(len(data)) != openedInfo.Size() {
		return durableLeaseJournal{}, fmt.Errorf("systemd property lease journal %s changed while it was read", s.path)
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return durableLeaseJournal{}, fmt.Errorf("parse systemd property lease journal %s: %w", s.path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var journal durableLeaseJournal
	if err := decoder.Decode(&journal); err != nil {
		return durableLeaseJournal{}, fmt.Errorf("parse systemd property lease journal %s: %w", s.path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return durableLeaseJournal{}, fmt.Errorf("parse systemd property lease journal %s: %w", s.path, err)
	}
	if err := validateDurableLeaseJournal(journal); err != nil {
		return durableLeaseJournal{}, fmt.Errorf("validate systemd property lease journal %s: %w", s.path, err)
	}
	return journal, nil
}

func (s *fileLeaseJournalStore) Save(journal durableLeaseJournal) error {
	sortDurableJournal(&journal)
	if err := validateDurableLeaseJournal(journal); err != nil {
		return fmt.Errorf("validate systemd property lease journal before persistence: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := ensurePrivateLeaseDirectory(dir, s.ownerUID); err != nil {
		return err
	}
	if len(journal.Units) == 0 {
		if info, err := os.Lstat(s.path); err == nil {
			if err := validateLeaseJournalInfo(s.path, info, s.ownerUID); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect empty systemd property lease journal %s: %w", s.path, err)
		}
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty systemd property lease journal %s: %w", s.path, err)
		}
		return syncDirectory(dir)
	}
	if info, err := os.Lstat(s.path); err == nil {
		if err := validateLeaseJournalInfo(s.path, info, s.ownerUID); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect systemd property lease journal %s before persistence: %w", s.path, err)
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode systemd property lease journal: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".resman-systemd-property-leases-*")
	if err != nil {
		return fmt.Errorf("create temporary systemd property lease journal: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary systemd property lease journal mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary systemd property lease journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary systemd property lease journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary systemd property lease journal: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace systemd property lease journal %s: %w", s.path, err)
	}
	return syncDirectory(dir)
}

func ensurePrivateLeaseDirectory(dir string, requiredUID int) error {
	if _, err := os.Lstat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect systemd property lease directory %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create systemd property lease directory %s: %w", dir, err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect systemd property lease directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("systemd property lease directory %s must be a non-symlink directory", dir)
	}
	uid, err := ownerUID(info)
	if err != nil {
		return err
	}
	if uid != requiredUID || info.Mode().Perm() != 0700 {
		return fmt.Errorf("systemd property lease directory %s must be owned by UID %d with mode 0700", dir, requiredUID)
	}
	return validateLeaseAncestors(dir, requiredUID)
}

func validateLeaseAncestors(dir string, requiredUID int) error {
	current, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve systemd property lease directory %s: %w", dir, err)
	}
	childUID := requiredUID
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		info, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("inspect systemd property lease ancestor %s: %w", parent, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("systemd property lease ancestor %s must be a non-symlink directory", parent)
		}
		uid, err := ownerUID(info)
		if err != nil {
			return err
		}
		untrustedOwnerCanReplace := uid != requiredUID && uid != 0 && info.Mode().Perm()&0200 != 0
		writableByGroupOrOther := info.Mode().Perm()&0022 != 0
		stickyProtectsChild := info.Mode()&os.ModeSticky != 0 && (childUID == requiredUID || childUID == 0)
		if untrustedOwnerCanReplace || (writableByGroupOrOther && !stickyProtectsChild) {
			return fmt.Errorf("systemd property lease ancestor %s can be replaced by an untrusted user", parent)
		}
		childUID = uid
		current = parent
	}
}

func validateLeaseJournalInfo(path string, info os.FileInfo, requiredUID int) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("systemd property lease journal %s must be a regular non-symlink file", path)
	}
	uid, err := ownerUID(info)
	if err != nil {
		return err
	}
	if uid != requiredUID || info.Mode().Perm() != 0600 {
		return fmt.Errorf("systemd property lease journal %s must be owned by UID %d with mode 0600", path, requiredUID)
	}
	return nil
}

func ownerUID(info os.FileInfo) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported file metadata type %T", info.Sys())
	}
	return int(stat.Uid), nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open systemd property lease directory %s for sync: %w", path, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync systemd property lease directory %s: %w", path, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close systemd property lease directory %s: %w", path, err)
	}
	return nil
}

func validateDurableLeaseJournal(journal durableLeaseJournal) error {
	if journal.Version != leaseJournalVersion {
		return fmt.Errorf("unsupported version %d", journal.Version)
	}
	if len(journal.Units) > 0 && journal.Generation == 0 {
		return fmt.Errorf("non-empty journal has zero generation")
	}
	if !sort.SliceIsSorted(journal.Units, func(left, right int) bool { return journal.Units[left].Unit < journal.Units[right].Unit }) {
		return fmt.Errorf("unit records are not in canonical order")
	}
	seenUnits := make(map[string]bool, len(journal.Units))
	for _, unit := range journal.Units {
		if seenUnits[unit.Unit] {
			return fmt.Errorf("duplicate unit %s", unit.Unit)
		}
		seenUnits[unit.Unit] = true
		if unit.Unit != parentUserSlice {
			if _, ok := parseUserSliceName(unit.Unit); !ok {
				return fmt.Errorf("invalid unit %q", unit.Unit)
			}
		}
		if unit.Identity.ControlGroupID == 0 || unit.Identity.InvocationID == strings.Repeat("0", 32) {
			return fmt.Errorf("unit %s has an invalid identity", unit.Unit)
		}
		invocationID, err := hex.DecodeString(unit.Identity.InvocationID)
		if err != nil || len(invocationID) != 16 {
			return fmt.Errorf("unit %s has an invalid invocation ID", unit.Unit)
		}
		if !strings.HasPrefix(unit.Identity.ObjectPath, "/org/freedesktop/systemd1/unit/") {
			return fmt.Errorf("unit %s has an invalid object path", unit.Unit)
		}
		switch unit.Phase {
		case leasePhaseApplying, leasePhaseApplied, leasePhaseRestoring, leasePhaseReloading:
		default:
			return fmt.Errorf("unit %s has invalid phase %q", unit.Unit, unit.Phase)
		}
		if len(unit.Properties) == 0 {
			return fmt.Errorf("unit %s has no property leases", unit.Unit)
		}
		if !sort.SliceIsSorted(unit.Properties, func(left, right int) bool { return unit.Properties[left].Property < unit.Properties[right].Property }) {
			return fmt.Errorf("unit %s properties are not in canonical order", unit.Unit)
		}
		seenProperties := make(map[PropertyName]bool, len(unit.Properties))
		uncertainProperties := 0
		for _, property := range unit.Properties {
			if seenProperties[property.Property] {
				return fmt.Errorf("unit %s has duplicate property %s", unit.Unit, property.Property)
			}
			seenProperties[property.Property] = true
			if _, ok := approvedScalarProperties[property.Property]; !ok {
				return fmt.Errorf("unit %s has invalid property %q", unit.Unit, property.Property)
			}
			for _, value := range []uint64{property.Baseline, property.PreviousApplied, property.LastApplied} {
				if err := validatePropertyValue(property.Property, value); err != nil {
					return fmt.Errorf("unit %s property %s has invalid value: %w", unit.Unit, property.Property, err)
				}
			}
			if property.Uncertain {
				uncertainProperties++
			}
			if property.NewLease && (!property.Uncertain || unit.Phase != leasePhaseApplying) {
				return fmt.Errorf("unit %s property %s has new_lease outside an uncertain apply", unit.Unit, property.Property)
			}
		}
		if unit.Phase == leasePhaseApplied && uncertainProperties != 0 {
			return fmt.Errorf("unit %s has uncertain properties in applied phase", unit.Unit)
		}
		if unit.Phase == leasePhaseApplying && uncertainProperties == 0 {
			return fmt.Errorf("unit %s has no uncertain property in applying phase", unit.Unit)
		}
		if (unit.Phase == leasePhaseRestoring || unit.Phase == leasePhaseReloading) && uncertainProperties != len(unit.Properties) {
			return fmt.Errorf("unit %s restore phase does not cover every property", unit.Unit)
		}
		if err := validateDurableFootprint(unit.Unit, unit.Footprint, unit.Phase == leasePhaseApplying); err != nil {
			return err
		}
		if err := validateDurableFootprint(unit.Unit, unit.PreviousFootprint, false); err != nil {
			return err
		}
		if unit.Phase != leasePhaseApplying && len(unit.PreviousFootprint) != 0 {
			return fmt.Errorf("unit %s retains a previous footprint outside applying phase", unit.Unit)
		}
		expectedPaths := make([]string, 0, len(unit.Properties))
		for _, property := range unit.Properties {
			expectedPaths = append(expectedPaths, managedRuntimeDropInPath(unit.Unit, property.Property))
		}
		sort.Strings(expectedPaths)
		if !equalStrings(durableFootprintPaths(unit.Footprint), expectedPaths) {
			return fmt.Errorf("unit %s footprint does not match its leased properties", unit.Unit)
		}
	}
	return nil
}

func validateDurableFootprint(unit string, footprint []durableFileFingerprint, allowPendingDigest bool) error {
	if !sort.SliceIsSorted(footprint, func(left, right int) bool { return footprint[left].Path < footprint[right].Path }) {
		return fmt.Errorf("unit %s footprint is not in canonical order", unit)
	}
	seen := make(map[string]bool, len(footprint))
	for _, file := range footprint {
		if seen[file.Path] {
			return fmt.Errorf("unit %s has duplicate footprint path %s", unit, file.Path)
		}
		seen[file.Path] = true
		root := filepath.Join("/run/systemd/system.control", unit+".d")
		relative, err := filepath.Rel(root, file.Path)
		if err != nil || relative == "." || strings.Contains(relative, string(filepath.Separator)) || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("unit %s has invalid footprint path %s", unit, file.Path)
		}
		if file.SHA256 == "" && allowPendingDigest {
			continue
		}
		digest, err := hex.DecodeString(file.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("unit %s footprint %s has invalid SHA-256", unit, file.Path)
		}
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("object field name is not a string")
			}
			if seen[name] {
				return fmt.Errorf("duplicate field %q", name)
			}
			seen[name] = true
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}

func sortDurableJournal(journal *durableLeaseJournal) {
	sort.Slice(journal.Units, func(left, right int) bool { return journal.Units[left].Unit < journal.Units[right].Unit })
	for index := range journal.Units {
		unit := &journal.Units[index]
		sort.Slice(unit.Properties, func(left, right int) bool { return unit.Properties[left].Property < unit.Properties[right].Property })
		sort.Slice(unit.Footprint, func(left, right int) bool { return unit.Footprint[left].Path < unit.Footprint[right].Path })
		sort.Slice(unit.PreviousFootprint, func(left, right int) bool {
			return unit.PreviousFootprint[left].Path < unit.PreviousFootprint[right].Path
		})
	}
}
