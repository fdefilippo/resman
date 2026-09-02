// Package limithook owns the privileged identity and filesystem boundary for
// limit-hook script execution.
package limithook

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ScriptIdentity is the resolved non-root identity used for one hook script.
type ScriptIdentity struct {
	Username string
	Group    string
	UID      uint32
	GID      uint32
}

// ResolveScriptIdentity resolves an exact user and group through the CGO-aware
// NSS boundary and rejects root credentials.
func ResolveScriptIdentity(username, groupName string) (ScriptIdentity, error) {
	if username == "" || groupName == "" {
		return ScriptIdentity{}, fmt.Errorf("script user and group must both be set")
	}
	resolvedUser, err := user.Lookup(username)
	if err != nil {
		return ScriptIdentity{}, fmt.Errorf("lookup script username %q through NSS: %w", username, err)
	}
	if resolvedUser.Username != username {
		return ScriptIdentity{}, fmt.Errorf("script username %q resolved as %q instead of an exact match", username, resolvedUser.Username)
	}
	uid, err := strconv.ParseUint(resolvedUser.Uid, 10, 32)
	if err != nil {
		return ScriptIdentity{}, fmt.Errorf("parse script UID %q for username %q: %w", resolvedUser.Uid, username, err)
	}
	resolvedGroup, err := user.LookupGroup(groupName)
	if err != nil {
		return ScriptIdentity{}, fmt.Errorf("lookup script group %q through NSS: %w", groupName, err)
	}
	if resolvedGroup.Name != groupName {
		return ScriptIdentity{}, fmt.Errorf("script group %q resolved as %q instead of an exact match", groupName, resolvedGroup.Name)
	}
	gid, err := strconv.ParseUint(resolvedGroup.Gid, 10, 32)
	if err != nil {
		return ScriptIdentity{}, fmt.Errorf("parse script GID %q for group %q: %w", resolvedGroup.Gid, groupName, err)
	}
	if uid == 0 || gid == 0 {
		return ScriptIdentity{}, fmt.Errorf("script identity must not use root UID or GID")
	}
	return ScriptIdentity{Username: username, Group: groupName, UID: uint32(uid), GID: uint32(gid)}, nil
}

// ValidateScriptPath verifies the executable and every ancestor without
// following symbolic links. Root or the configured script user must own each
// component, and no component may be writable by group or other users.
func ValidateScriptPath(path string, identity ScriptIdentity) error {
	if path == "" {
		return fmt.Errorf("script path is empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("script path %q is not absolute", path)
	}
	if clean := filepath.Clean(path); clean != path {
		return fmt.Errorf("script path %q is not clean; use %q", path, clean)
	}
	if strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("script path contains a NUL byte")
	}

	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(path, current), current)
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect script path component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("script path component %s is a symbolic link", current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("script path component %s has no Linux ownership metadata", current)
		}
		if stat.Uid != 0 && stat.Uid != identity.UID {
			return fmt.Errorf("script path component %s is owned by UID %d, not root or configured UID %d", current, stat.Uid, identity.UID)
		}
		trustedStickyAncestor := index < len(components)-1 && info.IsDir() && stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
		if info.Mode().Perm()&0o022 != 0 && !trustedStickyAncestor {
			return fmt.Errorf("script path component %s is writable by group or other users", current)
		}

		final := index == len(components)-1
		if final {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("script path %s is not a regular file", current)
			}
			if !identityCanExecute(info.Mode().Perm(), stat, identity) {
				return fmt.Errorf("script path %s is not executable by configured UID %d and GID %d", current, identity.UID, identity.GID)
			}
			continue
		}
		if !info.IsDir() {
			return fmt.Errorf("script path ancestor %s is not a directory", current)
		}
		if !identityCanTraverse(info.Mode().Perm(), stat, identity) {
			return fmt.Errorf("script path ancestor %s is not searchable by configured UID %d and GID %d", current, identity.UID, identity.GID)
		}
	}
	return nil
}

func identityCanExecute(mode os.FileMode, stat *syscall.Stat_t, identity ScriptIdentity) bool {
	return identityPermission(mode, stat, identity, 0o100, 0o010, 0o001)
}

func identityCanTraverse(mode os.FileMode, stat *syscall.Stat_t, identity ScriptIdentity) bool {
	return identityPermission(mode, stat, identity, 0o100, 0o010, 0o001)
}

func identityPermission(mode os.FileMode, stat *syscall.Stat_t, identity ScriptIdentity, owner, group, other os.FileMode) bool {
	switch {
	case stat.Uid == identity.UID:
		return mode&owner != 0
	case stat.Gid == identity.GID:
		return mode&group != 0
	default:
		return mode&other != 0
	}
}
