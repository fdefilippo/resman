package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// readConfigSource binds content and metadata to one non-symlink regular-file
// descriptor. The final path check refuses a replacement during the read; later
// persistence/reload confirmation still owns the compare-before-write boundary.
func readConfigSource(path string, open func(string, int, os.FileMode) (*os.File, error)) (info os.FileInfo, content []byte, exists bool, err error) {
	file, err := open(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, false, nil
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, false, fmt.Errorf("config path %s is a symbolic link; replace it with a regular file or select its target with --config: %w", path, err)
		}
		return nil, nil, false, fmt.Errorf("open config source %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err = file.Stat()
	if err != nil {
		return nil, nil, true, fmt.Errorf("inspect config source %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, true, fmt.Errorf("config path %s is not a regular file", path)
	}
	content, err = io.ReadAll(file)
	if err != nil {
		return nil, nil, true, fmt.Errorf("read config source %s: %w", path, err)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, true, fmt.Errorf("confirm config source descriptor %s: %w", path, err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, nil, true, fmt.Errorf("confirm config source path %s: %w", path, err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) ||
		int64(len(content)) != after.Size() || info.Size() != after.Size() ||
		!info.ModTime().Equal(after.ModTime()) || !after.ModTime().Equal(current.ModTime()) ||
		after.Size() != current.Size() {
		return nil, nil, true, fmt.Errorf("config source %s changed while it was read", path)
	}
	return after, content, true, nil
}
