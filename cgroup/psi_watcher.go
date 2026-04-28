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
	UID       int     // 0 for system-level, >0 for per-user cgroups
	Type      string  // "cpu", "io"
	SomeAvg10 float64 // avg10 percentage
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
// A wake pipe is included in the poll set so that AddMonitor/RemoveMonitor
// can interrupt a blocking poll() immediately.
type PSIWatcher struct {
	mu         sync.Mutex
	monitors   []*psiMonitor
	events     chan PSIEvent
	thresholds map[string]uint64 // typ -> stall threshold in microseconds
	windowUs   uint64
	wakeR      *os.File // read end of wake pipe (added to pollFds)
	wakeW      *os.File // write end (written to on AddMonitor/RemoveMonitor)
	done       chan struct{}
	wg         sync.WaitGroup
}

// NewPSIWatcher creates a watcher for pressure files.
// windowUs is the PSI tracking window in microseconds (e.g., 1000000 = 1s).
func NewPSIWatcher(windowUs uint64) *PSIWatcher {
	return &PSIWatcher{
		events:     make(chan PSIEvent, 64),
		thresholds: make(map[string]uint64),
		windowUs:   windowUs,
		done:       make(chan struct{}),
	}
}

// SetThreshold sets the stall threshold (microseconds) for a pressure type.
func (w *PSIWatcher) SetThreshold(typ string, stallUs uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.thresholds[typ] = stallUs
}

// Events returns the event channel.
func (w *PSIWatcher) Events() <-chan PSIEvent {
	return w.events
}

// AddMonitor registers a pressure file to monitor.
// uid: 0 for system-level, >0 for per-user cgroup
// typ: "cpu" or "io"
// pressurePath: full path to the pressure file
func (w *PSIWatcher) AddMonitor(uid int, typ string, pressurePath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, m := range w.monitors {
		if m.uid == uid && m.typ == typ && m.active {
			return nil
		}
	}

	threshold, ok := w.thresholds[typ]
	if !ok {
		return fmt.Errorf("no threshold configured for type %q", typ)
	}

	fd, err := os.OpenFile(pressurePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", pressurePath, err)
	}

	thresholdLine := fmt.Sprintf("some %d %d", threshold, w.windowUs)
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

	// Wake the poll loop so it picks up the new fd
	w.wake()

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

	w.wake()
}

// Start launches the poll loop goroutine.
func (w *PSIWatcher) Start() error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create wake pipe: %w", err)
	}
	w.wakeR = pr
	w.wakeW = pw

	w.wg.Add(1)
	go w.pollLoop()
	return nil
}

// Stop terminates the poll loop and all monitoring.
func (w *PSIWatcher) Stop() {
	close(w.done)

	w.mu.Lock()
	w.wake()
	w.mu.Unlock()

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
	if w.wakeW != nil {
		w.wakeW.Close()
		w.wakeW = nil
	}
}

// wake writes a byte to the wake pipe to interrupt poll().
// Must be called with w.mu held.
func (w *PSIWatcher) wake() {
	if w.wakeW == nil {
		return
	}
	w.wakeW.Write([]byte{0})
}

func (w *PSIWatcher) pollLoop() {
	defer w.wg.Done()

	// Drain wake pipe on exit
	defer func() {
		if w.wakeR != nil {
			w.wakeR.Close()
		}
	}()

	for {
		select {
		case <-w.done:
			return
		default:
		}

		w.mu.Lock()
		pollFds := make([]unix.PollFd, 0, len(w.monitors)+1)
		fdIndex := make(map[int32]int) // monitor fd -> index in monitors

		// Wake pipe is always first in the list
		pollFds = append(pollFds, unix.PollFd{
			Fd:     int32(w.wakeR.Fd()),
			Events: unix.POLLIN,
		})

		for i, m := range w.monitors {
			if !m.active {
				continue
			}
			fd := int32(m.fd.Fd())
			pollFds = append(pollFds, unix.PollFd{Fd: fd, Events: unix.POLLPRI})
			fdIndex[fd] = i
		}
		w.mu.Unlock()

		_, err := unix.Poll(pollFds, -1)
		if err != nil {
			select {
			case <-w.done:
				return
			default:
			}
			continue
		}

		// Check wake pipe first
		if pollFds[0].Revents&unix.POLLIN != 0 {
			var buf [8]byte
			w.wakeR.Read(buf[:])
			// After consuming the wake signal, re-enter the loop to rebuild pollFds
			select {
			case <-w.done:
				return
			default:
			}
			// Check if there are also PSI events to process before looping
		}

		// Process pressure events from remaining fds
		for i := 1; i < len(pollFds); i++ {
			pfd := pollFds[i]
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
	}
}
