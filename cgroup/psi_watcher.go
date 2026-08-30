package cgroup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/fdefilippo/resman/internal/operationgate"
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
	opGate     operationgate.Gate
	monitors   []*psiMonitor
	events     chan PSIEvent
	thresholds map[string]uint64 // typ -> stall threshold in microseconds
	windowUs   uint64
	wakeR      *os.File // read end of wake pipe (added to pollFds)
	wakeW      *os.File // write end (written to on AddMonitor/RemoveMonitor)
	done       chan struct{}
	wg         sync.WaitGroup
	openFile   func(string, int, os.FileMode) (*os.File, error)
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
	leaveOperation := w.opGate.Enter()
	defer leaveOperation()

	w.mu.Lock()
	for _, m := range w.monitors {
		if m.uid == uid && m.typ == typ && m.active {
			w.mu.Unlock()
			return nil
		}
	}

	threshold, ok := w.thresholds[typ]
	if !ok {
		w.mu.Unlock()
		return fmt.Errorf("no threshold configured for type %q", typ)
	}
	windowUs := w.windowUs
	w.mu.Unlock()

	openFile := w.openFile
	if openFile == nil {
		openFile = os.OpenFile
	}
	fd, err := openFile(pressurePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", pressurePath, err)
	}

	thresholdLine := fmt.Sprintf("some %d %d", threshold, windowUs)
	if _, err := fd.WriteString(thresholdLine); err != nil {
		_ = fd.Close()
		return fmt.Errorf("write threshold to %s: %w", pressurePath, err)
	}

	w.mu.Lock()
	w.monitors = append(w.monitors, &psiMonitor{
		uid:    uid,
		typ:    typ,
		path:   pressurePath,
		fd:     fd,
		active: true,
	})
	w.mu.Unlock()

	// Wake the poll loop so it picks up the new fd.
	w.wake()

	return nil
}

// RemoveMonitor unregisters a pressure file.
func (w *PSIWatcher) RemoveMonitor(uid int, typ string) {
	leaveOperation := w.opGate.Enter()
	defer leaveOperation()

	w.mu.Lock()
	var closeFiles []*os.File
	for _, m := range w.monitors {
		if m.uid == uid && m.typ == typ && m.active {
			m.active = false
			closeFiles = append(closeFiles, m.fd)
		}
	}
	w.compactInactiveMonitorsLocked()
	w.mu.Unlock()
	for _, fd := range closeFiles {
		_ = fd.Close()
	}

	w.wake()
}

// Start launches the poll loop goroutine.
func (w *PSIWatcher) Start() error {
	leaveOperation := w.opGate.Enter()
	defer leaveOperation()

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create wake pipe: %w", err)
	}
	w.mu.Lock()
	w.wakeR = pr
	w.wakeW = pw
	w.mu.Unlock()

	w.wg.Add(1)
	go w.pollLoop()
	return nil
}

// Stop terminates the poll loop and all monitoring.
func (w *PSIWatcher) Stop() {
	leaveOperation := w.opGate.Enter()
	defer leaveOperation()

	close(w.done)
	w.wake()

	w.wg.Wait()

	w.mu.Lock()
	var closeFiles []*os.File
	for _, m := range w.monitors {
		if m.active {
			closeFiles = append(closeFiles, m.fd)
			m.active = false
		}
	}
	w.monitors = nil
	if w.wakeW != nil {
		closeFiles = append(closeFiles, w.wakeW)
		w.wakeW = nil
	}
	w.mu.Unlock()
	for _, fd := range closeFiles {
		_ = fd.Close()
	}
}

// wake writes a byte to the wake pipe to interrupt poll().
func (w *PSIWatcher) wake() {
	w.mu.Lock()
	wakeW := w.wakeW
	w.mu.Unlock()
	if wakeW == nil {
		return
	}
	_, _ = wakeW.Write([]byte{0})
}

func (w *PSIWatcher) pollLoop() {
	defer w.wg.Done()

	// Drain wake pipe on exit
	defer func() {
		if w.wakeR != nil {
			_ = w.wakeR.Close()
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
		fdMonitors := make(map[int32]*psiMonitor)

		// Wake pipe is always first in the list
		pollFds = append(pollFds, unix.PollFd{
			Fd:     int32(w.wakeR.Fd()),
			Events: unix.POLLIN,
		})

		for _, m := range w.monitors {
			if !m.active {
				continue
			}
			fd := int32(m.fd.Fd())
			pollFds = append(pollFds, unix.PollFd{Fd: fd, Events: unix.POLLPRI})
			fdMonitors[fd] = m
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
			_, _ = w.wakeR.Read(buf[:])
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
			w.processPollEvent(pollFds[i], fdMonitors)
		}
	}
}

func (w *PSIWatcher) processPollEvent(pfd unix.PollFd, fdMonitors map[int32]*psiMonitor) {
	terminalEvents := int16(unix.POLLERR | unix.POLLNVAL | unix.POLLHUP)
	if pfd.Revents&terminalEvents != 0 {
		w.deactivatePolledMonitor(pfd.Fd, fdMonitors)
		return
	}
	if pfd.Revents&unix.POLLPRI == 0 {
		return
	}

	mon := w.activePolledMonitor(pfd.Fd, fdMonitors)
	if mon == nil {
		return
	}

	data := make([]byte, 4096)
	n, err := mon.fd.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		w.deactivateMonitor(mon)
		return
	}
	if n == 0 {
		w.deactivateMonitor(mon)
		return
	}

	stats, err := parsePSI(string(data[:n]))
	if err != nil {
		return
	}

	select {
	case w.events <- PSIEvent{
		UID:       mon.uid,
		Type:      mon.typ,
		SomeAvg10: stats.SomeAvg10,
		Timestamp: time.Now(),
	}:
	case <-w.done:
	default:
	}
}

func (w *PSIWatcher) activePolledMonitor(fd int32, fdMonitors map[int32]*psiMonitor) *psiMonitor {
	w.mu.Lock()
	defer w.mu.Unlock()

	mon, ok := fdMonitors[fd]
	if !ok || !mon.active {
		return nil
	}
	return mon
}

func (w *PSIWatcher) deactivatePolledMonitor(fd int32, fdMonitors map[int32]*psiMonitor) {
	if mon := w.activePolledMonitor(fd, fdMonitors); mon != nil {
		w.deactivateMonitor(mon)
	}
}

func (w *PSIWatcher) deactivateMonitor(mon *psiMonitor) {
	w.mu.Lock()
	if !mon.active {
		w.mu.Unlock()
		return
	}
	mon.active = false
	w.compactInactiveMonitorsLocked()
	w.mu.Unlock()
	_ = mon.fd.Close()
}

func (w *PSIWatcher) compactInactiveMonitorsLocked() {
	activeCount := 0
	for _, mon := range w.monitors {
		if mon.active {
			w.monitors[activeCount] = mon
			activeCount++
		}
	}
	clear(w.monitors[activeCount:])
	w.monitors = w.monitors[:activeCount]
}
