package cgroup

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPSIWatcherDeactivatesMonitorOnTerminalPollEvent(t *testing.T) {
	events := []int16{
		unix.POLLERR,
		unix.POLLNVAL,
		unix.POLLHUP,
		unix.POLLPRI | unix.POLLERR,
	}

	for _, revents := range events {
		t.Run(pollEventName(revents), func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "pressure")
			if err != nil {
				t.Fatalf("failed to create pressure fixture: %v", err)
			}

			watcher := NewPSIWatcher(1_000_000)
			monitor := &psiMonitor{uid: 1000, typ: "cpu", fd: file, active: true}
			watcher.monitors = []*psiMonitor{monitor}
			fd := int32(file.Fd())

			watcher.processPollEvent(
				unix.PollFd{Fd: fd, Revents: revents},
				map[int32]int{fd: 0},
			)

			if monitor.active {
				t.Fatal("monitor remained active after terminal poll event")
			}
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("monitor file was not closed: %v", err)
			}
		})
	}
}

func TestPSIWatcherDeactivatesMonitorOnReadError(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pressure")
	if err != nil {
		t.Fatalf("failed to create pressure fixture: %v", err)
	}
	fd := int32(file.Fd())
	if err := file.Close(); err != nil {
		t.Fatalf("failed to close pressure fixture: %v", err)
	}

	watcher := NewPSIWatcher(1_000_000)
	monitor := &psiMonitor{uid: 1000, typ: "cpu", fd: file, active: true}
	watcher.monitors = []*psiMonitor{monitor}

	watcher.processPollEvent(
		unix.PollFd{Fd: fd, Revents: unix.POLLPRI},
		map[int32]int{fd: 0},
	)

	if monitor.active {
		t.Fatal("monitor remained active after pressure file read failed")
	}
}

func pollEventName(revents int16) string {
	switch revents {
	case unix.POLLERR:
		return "POLLERR"
	case unix.POLLNVAL:
		return "POLLNVAL"
	case unix.POLLHUP:
		return "POLLHUP"
	default:
		return "POLLPRI_with_POLLERR"
	}
}
