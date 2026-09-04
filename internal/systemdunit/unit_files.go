package systemdunit

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

type unitFileInspector interface {
	mutablePaths(string) ([]string, error)
	fingerprints([]string) ([]unitFileFingerprint, error)
}

type unitFileFingerprint struct {
	path   string
	digest [sha256.Size]byte
}

type localUnitFileInspector struct {
	roots []string
}

func (i localUnitFileInspector) mutablePaths(unit string) ([]string, error) {
	if unit != parentUserSlice {
		if _, ok := parseUserSliceName(unit); !ok {
			return nil, fmt.Errorf("unit %q is outside the canonical user-slice boundary", unit)
		}
	}
	var result []string
	roots := i.roots
	if len(roots) == 0 {
		roots = []string{
			"/etc/systemd/system",
			"/run/systemd/system",
			"/etc/systemd/system.control",
			"/run/systemd/system.control",
			"/run/systemd/transient",
		}
	}
	for _, root := range roots {
		path := filepath.Join(root, unit)
		exists, err := appendExistingPath(&result, path)
		if err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		dropInDirectory := path + ".d"
		info, err := os.Lstat(dropInDirectory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect mutable unit drop-in directory %s: %w", dropInDirectory, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			result = append(result, dropInDirectory)
			continue
		}
		entries, err := os.ReadDir(dropInDirectory)
		if err != nil {
			return nil, fmt.Errorf("inspect mutable unit drop-ins in %s: %w", dropInDirectory, err)
		}
		for _, entry := range entries {
			result = append(result, filepath.Join(dropInDirectory, entry.Name()))
		}
	}
	sort.Strings(result)
	return result, nil
}

func (i localUnitFileInspector) fingerprints(paths []string) ([]unitFileFingerprint, error) {
	result := make([]unitFileFingerprint, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect mutable unit file %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("mutable unit file %s is not a regular non-symlink file", path)
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open mutable unit file %s without following links: %w", path, err)
		}
		openedBefore, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("inspect opened mutable unit file %s: %w", path, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, io.LimitReader(file, 1<<20))
		openedInfo, statErr := file.Stat()
		closeErr := file.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("read mutable unit file %s: %w", path, copyErr)
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect opened mutable unit file %s: %w", path, statErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close mutable unit file %s: %w", path, closeErr)
		}
		pathInfo, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("reinspect mutable unit file %s: %w", path, err)
		}
		if info.Size() > 1<<20 || openedBefore.Size() != openedInfo.Size() || !openedBefore.ModTime().Equal(openedInfo.ModTime()) || !os.SameFile(info, openedBefore) || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, pathInfo) {
			return nil, fmt.Errorf("mutable unit file %s changed identity or size while it was read", path)
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		result = append(result, unitFileFingerprint{path: path, digest: digest})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].path < result[right].path })
	return result, nil
}

func appendExistingPath(paths *[]string, path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		*paths = append(*paths, path)
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect mutable unit file %s: %w", path, err)
}
