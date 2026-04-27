package cgroup

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// PSIEvent represents a pressure stall event from a monitored cgroup.
type PSIEvent struct {
	UID       int       // 0 for system-level, >0 for per-user cgroups
	Type      string    // "cpu", "io"
	SomeAvg10 float64   // avg10 percentage
	Timestamp time.Time
}

type psiMonitor struct {
	uid    int
	typ    string
	path   string
	fd     *os.File
	active bool
}

// PSIWatcher monitors pressure files via poll() with dynamic per-user cgroup support.
// Uses a single poll loop to monitor all registered pressure files efficiently.
type PSIWatcher struct {
	mu        sync.Mutex
	monitors  []*psiMonitor
	events    chan PSIEvent
	update    chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	threshold uint64
	windowUs  uint64
}

// NewPSIWatcher creates a watcher for pressure files.
// thresholdUs is the stall threshold in microseconds per windowUs window.
// e.g., 50000us on 1000000us window = 5% pressure.
func NewPSIWatcher(thresholdUs, windowUs uint64) *PSIWatcher {
	return &PSIWatcher{
		events:    make(chan PSIEvent, 64),
		update:    make(chan struct{}, 1),
		done:      make(chan struct{}),
		threshold: thresholdUs,
		windowUs:  windowUs,
	}
}

// Events returns the event channel.
func (w *PSIWatcher) Events() <-chan PSIEvent {
	return w.events
}

// AddMonitor registers a pressure file to monitor.
// uid: 0 for system-level, >0 for per-user cgroup
// typ: "cpu" or "io"
// pressurePath: full path to the pressure file (e.g., /sys/fs/cgroup/resman/user_1000/cpu.pressure)
func (w *PSIWatcher) AddMonitor(uid int, typ string, pressurePath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Check if already registered
	for _, m := range w.monitors {
		if m.uid == uid && m.typ == typ && m.active {
			return nil
		}
	}

	fd, err := os.OpenFile(pressurePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", pressurePath, err)
	}

	thresholdLine := fmt.Sprintf("some %d %d", w.threshold, w.windowUs)
	if _, err := fd.WriteString(thresholdLine); err != nil {
		fd.Close()
		return fmt.Errorf("write threshold to %s: %w", pressurePath, err)
	}

	w.monitors = append(w.monitors, &psiMonitor{
		uid:    uid,
		typ:    typ,
		path:   pressurePath,
		fd:     fd,
		active: true,
	})

	// Signal poll loop to refresh its fd list
	select {
	case w.update <- struct{}{}:
	default:
	}

	return nil
}

// RemoveMonitor unregisters a pressure file.
func (w *PSIWatcher) RemoveMonitor(uid int, typ string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, m := range w.monitors {
		if m.uid == uid && m.typ == typ && m.active {
			m.active = false
			m.fd.Close()
		}
	}

	select {
	case w.update <- struct{}{}:
	default:
	}
}

// Start launches the poll loop goroutine.
func (w *PSIWatcher) Start() {
	w.wg.Add(1)
	go w.pollLoop()
}

// Stop terminates the poll loop and all monitoring.
func (w *PSIWatcher) Stop() {
	close(w.done)
	w.wg.Wait()

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.monitors {
		if m.active {
			m.fd.Close()
			m.active = false
		}
	}
	w.monitors = nil
}

func (w *PSIWatcher) pollLoop() {
	defer w.wg.Done()

	for {
		select {
		case <-w.done:
			return
		default:
		}

		w.mu.Lock()
		pollFds := make([]unix.PollFd, 0, len(w.monitors))
		fdIndex := make(map[int32]int) // fd -> index in monitors
		for i, m := range w.monitors {
			if !m.active {
				continue
			}
			fd := int32(m.fd.Fd())
			pollFds = append(pollFds, unix.PollFd{Fd: fd, Events: unix.POLLPRI})
			fdIndex[fd] = i
		}
		w.mu.Unlock()

		if len(pollFds) == 0 {
			select {
			case <-w.done:
				return
			case <-w.update:
				continue
			case <-time.After(10 * time.Second):
				continue
			}
		}

		_, err := unix.Poll(pollFds, -1)
		if err != nil {
			select {
			case <-w.done:
				return
			default:
			}
			continue
		}

		for _, pfd := range pollFds {
			if pfd.Revents&unix.POLLPRI == 0 && pfd.Revents&unix.POLLERR == 0 {
				continue
			}

			w.mu.Lock()
			idx, ok := fdIndex[pfd.Fd]
			if !ok || idx >= len(w.monitors) || !w.monitors[idx].active {
				w.mu.Unlock()
				continue
			}
			mon := w.monitors[idx]
			w.mu.Unlock()

			data := make([]byte, 4096)
			n, err := mon.fd.ReadAt(data, 0)
			if err != nil {
				continue
			}

			stats, err := parsePSI(string(data[:n]))
			if err != nil {
				continue
			}

			select {
			case w.events <- PSIEvent{
				UID:       mon.uid,
				Type:      mon.typ,
				SomeAvg10: stats.SomeAvg10,
				Timestamp: time.Now(),
			}:
			case <-w.done:
				return
			default:
			}
		}

		// Short yield to allow updates to be processed on the next iteration
		select {
		case <-w.done:
			return
		case <-w.update:
		default:
		}
	}
}
