package ioweights

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const policyMapFileMode = os.FileMode(0600)

// PolicySyntaxError identifies malformed weighted-I/O map content.
type PolicySyntaxError struct{ Cause error }

func (e *PolicySyntaxError) Error() string { return e.Cause.Error() }
func (e *PolicySyntaxError) Unwrap() error { return e.Cause }

// PolicyIdentityError identifies an unsafe or ambiguous exact NSS result.
type PolicyIdentityError struct {
	Username string
	Cause    error
}

func (e *PolicyIdentityError) Error() string { return e.Cause.Error() }
func (e *PolicyIdentityError) Unwrap() error { return e.Cause }

// PolicyInputs are the complete typed inputs for one immutable snapshot.
type PolicyInputs struct {
	Root, Default Weight
	MapPath       PolicyMapPath
}

// PolicyLoader validates safe-file identity, syntax and exact NSS identities
// before publishing a detached immutable snapshot.
type PolicyLoader struct {
	openFile     func(string, int, os.FileMode) (*os.File, error)
	lstat        func(string) (os.FileInfo, error)
	effectiveUID func() int
	ownerUID     func(os.FileInfo) (int, error)
}

// NewPolicyLoader constructs the production safe-file loader.
func NewPolicyLoader() *PolicyLoader {
	return &PolicyLoader{openFile: os.OpenFile, lstat: os.Lstat, effectiveUID: os.Geteuid, ownerUID: policyFileOwnerUID}
}

// Load reads and resolves one complete policy candidate.
func (l *PolicyLoader) Load(inputs PolicyInputs, resolver ExactIdentityResolver) (PolicySnapshot, error) {
	if l == nil || l.openFile == nil || l.lstat == nil || l.effectiveUID == nil || l.ownerUID == nil {
		return PolicySnapshot{}, fmt.Errorf("weighted I/O policy loader is not initialized")
	}
	if resolver == nil {
		return PolicySnapshot{}, fmt.Errorf("exact username resolver is required")
	}
	path, err := NewPolicyMapPath(inputs.MapPath.String())
	if err != nil {
		return PolicySnapshot{}, err
	}
	data, source, err := l.readSafePolicyFile(path)
	if err != nil {
		return PolicySnapshot{}, err
	}
	return l.loadContent(inputs, data, source, resolver)
}

// LoadContent validates detached content through the production grammar.
func (l *PolicyLoader) LoadContent(inputs PolicyInputs, data []byte, resolver ExactIdentityResolver) (PolicySnapshot, error) {
	if l == nil || resolver == nil {
		return PolicySnapshot{}, fmt.Errorf("initialized loader and exact username resolver are required")
	}
	path, err := NewPolicyMapPath(inputs.MapPath.String())
	if err != nil {
		return PolicySnapshot{}, err
	}
	return l.loadContent(inputs, data, PolicySource{path: path, size: int64(len(data)), digest: sha256.Sum256(data)}, resolver)
}

func (l *PolicyLoader) loadContent(inputs PolicyInputs, data []byte, source PolicySource, resolver ExactIdentityResolver) (PolicySnapshot, error) {
	root, err := NewWeight(inputs.Root.Value())
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("validate root weight: %w", err)
	}
	defaultIO, err := NewWeight(inputs.Default.Value())
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("validate default weight: %w", err)
	}
	raw, err := parsePolicyMap(data)
	if err != nil {
		return PolicySnapshot{}, &PolicySyntaxError{Cause: err}
	}
	entries := make([]UserWeight, 0, len(raw))
	byUID := make(map[int]UserWeight, len(raw))
	for _, candidate := range raw {
		identities, resolveErr := resolver.ResolveExactUsername(candidate.username)
		if resolveErr != nil || len(identities) != 1 || identities[0].Username != candidate.username || identities[0].UID <= 0 {
			cause := resolveErr
			if cause == nil {
				cause = fmt.Errorf("username %q must resolve exactly once to a non-root UID", candidate.username)
			}
			return PolicySnapshot{}, &PolicyIdentityError{Username: candidate.username, Cause: cause}
		}
		identity := identities[0]
		if previous, duplicate := byUID[identity.UID]; duplicate {
			return PolicySnapshot{}, &PolicyIdentityError{Username: candidate.username, Cause: fmt.Errorf("usernames %q and %q resolve to duplicate UID %d", previous.username, candidate.username, identity.UID)}
		}
		entry := UserWeight{username: candidate.username, uid: identity.UID, weight: candidate.weight, line: candidate.line}
		entries = append(entries, entry)
		byUID[identity.UID] = entry
	}
	return PolicySnapshot{root: root, defaultIO: defaultIO, entries: entries, byUID: byUID, source: source}, nil
}

// ConfirmSource proves that a validated source still names the same safe bytes.
func (l *PolicyLoader) ConfirmSource(source PolicySource) error {
	data, current, err := l.readSafePolicyFile(source.path)
	if err != nil {
		return fmt.Errorf("confirm weighted I/O map source: %w", err)
	}
	if current.dev != source.dev || current.inode != source.inode || current.size != source.size || current.digest != source.digest || sha256.Sum256(data) != source.digest {
		return fmt.Errorf("weighted I/O map %s changed after candidate validation", source.path.String())
	}
	return nil
}

func (l *PolicyLoader) readSafePolicyFile(path PolicyMapPath) ([]byte, PolicySource, error) {
	if err := l.validateAncestors(path); err != nil {
		return nil, PolicySource{}, err
	}
	file, err := l.openFile(path.String(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, PolicySource{}, fmt.Errorf("open weighted I/O map %s without following symbolic links: %w", path.String(), err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, PolicySource{}, fmt.Errorf("inspect opened weighted I/O map %s: %w", path.String(), err)
	}
	if err := l.validateOpenedFile(path, info); err != nil {
		_ = file.Close()
		return nil, PolicySource{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaximumPolicyMapBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return nil, PolicySource{}, fmt.Errorf("read weighted I/O map %s: %w", path.String(), firstError(readErr, statErr, closeErr))
	}
	if len(data) > MaximumPolicyMapBytes || info.Size() != after.Size() || int64(len(data)) != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return nil, PolicySource{}, fmt.Errorf("weighted I/O map %s changed or exceeded bounds while being read", path.String())
	}
	if err := l.validateAncestors(path); err != nil {
		return nil, PolicySource{}, err
	}
	pathInfo, err := l.lstat(path.String())
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(after, pathInfo) {
		return nil, PolicySource{}, fmt.Errorf("weighted I/O map path %s changed identity while being read", path.String())
	}
	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, PolicySource{}, fmt.Errorf("weighted I/O map %s has unsupported metadata", path.String())
	}
	return data, PolicySource{path: path, dev: uint64(stat.Dev), inode: stat.Ino, size: after.Size(), digest: sha256.Sum256(data)}, nil
}

func firstError(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *PolicyLoader) validateOpenedFile(path PolicyMapPath, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm() != policyMapFileMode {
		return fmt.Errorf("weighted I/O map %s must be a regular file with mode 0600", path.String())
	}
	owner, err := l.ownerUID(info)
	if err != nil {
		return err
	}
	if owner != 0 && owner != l.effectiveUID() {
		return fmt.Errorf("weighted I/O map %s is owned by untrusted UID %d", path.String(), owner)
	}
	return nil
}

func (l *PolicyLoader) validateAncestors(path PolicyMapPath) error {
	euid := l.effectiveUID()
	for current := filepath.Dir(path.String()); ; current = filepath.Dir(current) {
		info, err := l.lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("weighted I/O map ancestor %s is not a safe directory", current)
		}
		owner, err := l.ownerUID(info)
		if err != nil {
			return err
		}
		if owner != 0 && owner != euid {
			return fmt.Errorf("weighted I/O map ancestor %s is owned by untrusted UID %d", current, owner)
		}
		if info.Mode().Perm()&0022 != 0 && (info.Mode()&os.ModeSticky == 0 || owner != 0) {
			return fmt.Errorf("weighted I/O map ancestor %s is writable by group or other users", current)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func policyFileOwnerUID(info os.FileInfo) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported file metadata type %T", info.Sys())
	}
	return int(stat.Uid), nil
}
