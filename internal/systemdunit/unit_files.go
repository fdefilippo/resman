package systemdunit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type unitFileInspector interface {
	mutablePaths(string) ([]string, error)
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
