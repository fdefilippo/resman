package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPSIWatcherStateRemainsAvailableWhilePressureFileOpenBlocks(t *testing.T) {
	watcher := NewPSIWatcher(1_000_000)
	watcher.SetThreshold("cpu", 50_000)
	started := make(chan struct{})
	release := make(chan struct{})
	watcher.openFile = func(path string, flag int, mode os.FileMode) (*os.File, error) {
		close(started)
		<-release
		return os.OpenFile(path, flag, mode)
	}
	pressurePath := filepath.Join(t.TempDir(), "cpu.pressure")
	if err := os.WriteFile(pressurePath, nil, 0600); err != nil {
		t.Fatalf("write pressure fixture: %v", err)
	}

	addDone := make(chan error, 1)
	go func() { addDone <- watcher.AddMonitor(1000, "cpu", pressurePath) }()
	<-started
	thresholdDone := make(chan struct{})
	go func() {
		watcher.SetThreshold("io", 25_000)
		close(thresholdDone)
	}()
	select {
	case <-thresholdDone:
	case <-time.After(time.Second):
		close(release)
		<-addDone
		t.Fatal("SetThreshold() blocked behind pressure-file open")
	}
	close(release)
	if err := <-addDone; err != nil {
		t.Fatalf("AddMonitor() error: %v", err)
	}
	watcher.RemoveMonitor(1000, "cpu")
}

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
				map[int32]*psiMonitor{fd: monitor},
			)

			if monitor.active {
				t.Fatal("monitor remained active after terminal poll event")
			}
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("monitor file was not closed: %v", err)
			}
			if len(watcher.monitors) != 0 {
				t.Fatalf("inactive monitor was retained: %d entries", len(watcher.monitors))
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
		map[int32]*psiMonitor{fd: monitor},
	)

	if monitor.active {
		t.Fatal("monitor remained active after pressure file read failed")
	}
	if len(watcher.monitors) != 0 {
		t.Fatalf("inactive monitor was retained: %d entries", len(watcher.monitors))
	}
}

func TestPSIWatcherEmitsEventFromSomeOnlyCPUPressure(t *testing.T) {
	pressurePath := filepath.Join(t.TempDir(), "cpu.pressure")
	content := "some avg10=3.25 avg60=2.00 avg300=1.00 total=12345\n"
	if err := os.WriteFile(pressurePath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create pressure fixture: %v", err)
	}
	file, err := os.Open(pressurePath)
	if err != nil {
		t.Fatalf("failed to open pressure fixture: %v", err)
	}
	defer func() { _ = file.Close() }()

	watcher := NewPSIWatcher(1_000_000)
	monitor := &psiMonitor{uid: 1000, typ: "cpu", path: pressurePath, fd: file, active: true}
	watcher.monitors = []*psiMonitor{monitor}
	fd := int32(file.Fd())

	watcher.processPollEvent(
		unix.PollFd{Fd: fd, Revents: unix.POLLPRI},
		map[int32]*psiMonitor{fd: monitor},
	)

	select {
	case event := <-watcher.Events():
		if event.UID != 1000 || event.Type != "cpu" || event.SomeAvg10 != 3.25 {
			t.Fatalf("unexpected PSI event: %+v", event)
		}
	default:
		t.Fatal("some-only CPU pressure did not emit an event")
	}
}

func TestPSIWatcherDoesNotRetainRemovedMonitors(t *testing.T) {
	pressurePath := filepath.Join(t.TempDir(), "cpu.pressure")
	if err := os.WriteFile(pressurePath, []byte("some avg10=0 avg60=0 avg300=0 total=0\n"), 0644); err != nil {
		t.Fatalf("failed to create pressure fixture: %v", err)
	}

	watcher := NewPSIWatcher(1_000_000)
	watcher.SetThreshold("cpu", 50_000)

	for i := 0; i < 100; i++ {
		if err := watcher.AddMonitor(1000, "cpu", pressurePath); err != nil {
			t.Fatalf("AddMonitor() iteration %d error: %v", i, err)
		}
		if len(watcher.monitors) != 1 {
			t.Fatalf("monitor count after add = %d, want 1", len(watcher.monitors))
		}
		watcher.RemoveMonitor(1000, "cpu")
		if len(watcher.monitors) != 0 {
			t.Fatalf("monitor count after remove = %d, want 0", len(watcher.monitors))
		}
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
