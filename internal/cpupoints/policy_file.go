package cpupoints

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const policyMapFileMode = os.FileMode(0600)

// PolicyLoader builds an immutable snapshot using filesystem and NSS I/O before
// any caller publishes it. The production file contract is an absolute clean
// path, a non-symlink regular file owned by root or the daemon EUID with exact
// mode 0600, and ancestors owned by root or the daemon EUID that are not
// group/other-writable unless a root-owned sticky directory protects the entry.
type PolicyLoader struct {
	openFile     func(string, int, os.FileMode) (*os.File, error)
	lstat        func(string) (os.FileInfo, error)
	effectiveUID func() int
	ownerUID     func(os.FileInfo) (int, error)
}

// NewPolicyLoader constructs the production safe-file boundary.
func NewPolicyLoader() *PolicyLoader {
	return &PolicyLoader{
		openFile:     os.OpenFile,
		lstat:        os.Lstat,
		effectiveUID: os.Geteuid,
		ownerUID:     policyFileOwnerUID,
	}
}

// Load reads, parses and resolves a complete policy without mutating live state.
func (l *PolicyLoader) Load(inputs PolicyInputs, resolver ExactIdentityResolver) (PolicySnapshot, error) {
	if l == nil || l.openFile == nil || l.lstat == nil || l.effectiveUID == nil || l.ownerUID == nil {
		return PolicySnapshot{}, fmt.Errorf("policy loader is not initialized; use NewPolicyLoader")
	}
	if resolver == nil {
		return PolicySnapshot{}, fmt.Errorf("exact username resolver is required")
	}
	reserve, err := NewReservePoints(inputs.Reserve.Value())
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("validate policy reserve: %w", err)
	}
	bestEffort, err := NewBestEffortPoints(inputs.BestEffort.Value())
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("validate policy best-effort entitlement: %w", err)
	}
	mapPath, err := NewPolicyMapPath(inputs.MapPath.String())
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("validate policy map path: %w", err)
	}

	data, source, err := l.readSafePolicyFile(mapPath)
	if err != nil {
		return PolicySnapshot{}, err
	}
	rawEntries, err := parsePolicyMap(data)
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("parse CPU Points map %s: %w", mapPath.String(), err)
	}

	pool := reserve.ParentPool()
	entries := make([]UserGuarantee, 0, len(rawEntries))
	guaranteesByUID := make(map[int]UserGuarantee, len(rawEntries))
	var configuredTotal uint64
	for _, raw := range rawEntries {
		identities, err := resolver.ResolveExactUsername(raw.username)
		if err != nil {
			return PolicySnapshot{}, fmt.Errorf("resolve CPU Points map %s line %d username %q: %w", mapPath.String(), raw.line, raw.username, err)
		}
		if len(identities) == 0 {
			return PolicySnapshot{}, fmt.Errorf("CPU Points map %s line %d username %q did not resolve through NSS", mapPath.String(), raw.line, raw.username)
		}
		if len(identities) != 1 {
			return PolicySnapshot{}, fmt.Errorf("CPU Points map %s line %d username %q resolved to %d identities; exactly one is required", mapPath.String(), raw.line, raw.username, len(identities))
		}
		identity := identities[0]
		if identity.Username != raw.username {
			return PolicySnapshot{}, fmt.Errorf("CPU Points map %s line %d username %q resolved as non-exact identity %q", mapPath.String(), raw.line, raw.username, identity.Username)
		}
		if identity.UID < 0 {
			return PolicySnapshot{}, fmt.Errorf("CPU Points map %s line %d username %q resolved to unrepresentable UID %d", mapPath.String(), raw.line, raw.username, identity.UID)
		}
		if previous, duplicate := guaranteesByUID[identity.UID]; duplicate {
			return PolicySnapshot{}, fmt.Errorf("CPU Points map %s usernames %q and %q resolve to duplicate UID %d", mapPath.String(), previous.Username(), raw.username, identity.UID)
		}

		configuredTotal += raw.points.Value()
		if configuredTotal+bestEffort.Value() > pool.Value() {
			return PolicySnapshot{}, fmt.Errorf("CPU Points policy overcommits nominal pool %d: configured guarantees %d plus best effort %d", pool.Value(), configuredTotal, bestEffort.Value())
		}
		guarantee := UserGuarantee{
			username: raw.username,
			uid:      identity.UID,
			points:   raw.points,
			line:     raw.line,
		}
		entries = append(entries, guarantee)
		guaranteesByUID[identity.UID] = guarantee
	}

	total, err := NewConfiguredGuaranteeTotalPoints(configuredTotal)
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("validate configured guarantee total: %w", err)
	}
	return PolicySnapshot{
		reserve:         reserve,
		pool:            pool,
		bestEffort:      bestEffort,
		configuredTotal: total,
		entries:         entries,
		guaranteesByUID: guaranteesByUID,
		source:          source,
	}, nil
}

func (l *PolicyLoader) readSafePolicyFile(path PolicyMapPath) ([]byte, PolicySource, error) {
	if err := l.validateAncestors(path); err != nil {
		return nil, PolicySource{}, err
	}
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	file, err := l.openFile(path.String(), flags, 0)
	if err != nil {
		return nil, PolicySource{}, fmt.Errorf("open CPU Points map %s without following symbolic links: %w", path.String(), err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, PolicySource{}, fmt.Errorf("inspect opened CPU Points map %s: %w", path.String(), err)
	}
	if _, err := l.validateOpenedFile(path, info); err != nil {
		_ = file.Close()
		return nil, PolicySource{}, err
	}
	if info.Size() > MaximumPolicyMapBytes {
		_ = file.Close()
		return nil, PolicySource{}, fmt.Errorf("CPU Points map %s is %d bytes; maximum is %d", path.String(), info.Size(), MaximumPolicyMapBytes)
	}

	data, readErr := io.ReadAll(io.LimitReader(file, MaximumPolicyMapBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return nil, PolicySource{}, fmt.Errorf("read CPU Points map %s: %w", path.String(), readErr)
	}
	if statErr != nil {
		return nil, PolicySource{}, fmt.Errorf("reinspect opened CPU Points map %s: %w", path.String(), statErr)
	}
	if closeErr != nil {
		return nil, PolicySource{}, fmt.Errorf("close CPU Points map %s: %w", path.String(), closeErr)
	}
	if len(data) > MaximumPolicyMapBytes {
		return nil, PolicySource{}, fmt.Errorf("CPU Points map %s exceeds the %d-byte maximum while being read", path.String(), MaximumPolicyMapBytes)
	}
	if info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return nil, PolicySource{}, fmt.Errorf("CPU Points map %s changed while it was being read", path.String())
	}
	if err := l.validateAncestors(path); err != nil {
		return nil, PolicySource{}, err
	}
	pathInfo, err := l.lstat(path.String())
	if err != nil {
		return nil, PolicySource{}, fmt.Errorf("reinspect CPU Points map path %s: %w", path.String(), err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(after, pathInfo) {
		return nil, PolicySource{}, fmt.Errorf("CPU Points map path %s changed identity while it was being read", path.String())
	}

	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, PolicySource{}, fmt.Errorf("CPU Points map %s has unsupported file metadata type %T", path.String(), after.Sys())
	}
	return data, PolicySource{
		path:  path,
		dev:   uint64(stat.Dev),
		inode: stat.Ino,
		size:  after.Size(),
	}, nil
}

func (l *PolicyLoader) validateOpenedFile(path PolicyMapPath, info os.FileInfo) (int, error) {
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("CPU Points map %s is not a regular file", path.String())
	}
	if info.Mode().Perm() != policyMapFileMode {
		return 0, fmt.Errorf("CPU Points map %s has mode %04o; set mode 0600 before restarting resman", path.String(), info.Mode().Perm())
	}
	owner, err := l.ownerUID(info)
	if err != nil {
		return 0, fmt.Errorf("inspect CPU Points map ownership for %s: %w", path.String(), err)
	}
	euid := l.effectiveUID()
	if owner != 0 && owner != euid {
		return 0, fmt.Errorf("CPU Points map %s is owned by UID %d; change ownership to root or daemon UID %d before restarting resman", path.String(), owner, euid)
	}
	return owner, nil
}

func (l *PolicyLoader) validateAncestors(path PolicyMapPath) error {
	euid := l.effectiveUID()
	current := filepath.Dir(path.String())
	for {
		info, err := l.lstat(current)
		if err != nil {
			return fmt.Errorf("inspect CPU Points map ancestor %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("CPU Points map ancestor %s is a symbolic link", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("CPU Points map ancestor %s is not a directory", current)
		}
		owner, err := l.ownerUID(info)
		if err != nil {
			return fmt.Errorf("inspect CPU Points map ancestor ownership for %s: %w", current, err)
		}
		if owner != 0 && owner != euid {
			return fmt.Errorf("CPU Points map ancestor %s is owned by untrusted UID %d", current, owner)
		}
		writableByOthers := info.Mode().Perm()&0022 != 0
		stickyRootBoundary := info.Mode()&os.ModeSticky != 0 && owner == 0
		if writableByOthers && !stickyRootBoundary {
			return fmt.Errorf("CPU Points map ancestor %s is writable by group or other users", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func policyFileOwnerUID(info os.FileInfo) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported file metadata type %T", info.Sys())
	}
	return int(stat.Uid), nil
}
