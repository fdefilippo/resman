package cgroup

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// PSIEvent rappresenta un evento di pressure stall.
type PSIEvent struct {
	Type      string    // "cpu", "io", "memory"
	SomeAvg10 float64   // % media 10s
	Timestamp time.Time
}

// PSIWatcher monitora i file pressure tramite poll().
// Quando la pressione supera la soglia, invia un evento sul canale.
type PSIWatcher struct {
	mu       sync.Mutex
	events   chan PSIEvent
	done     chan struct{}
	wg       sync.WaitGroup

	cgroupRoot string
	thresholds map[string]uint64 // tipo -> soglia in microsec
	windowUs   uint64
}

// NewPSIWatcher crea un watcher per i pressure file.
// threshold è in microsecondi di stall per window.
// Ad esempio 50000 us su window 1000000 us = 5% di pressure.
func NewPSIWatcher(cgroupRoot string, windowUs uint64) *PSIWatcher {
	return &PSIWatcher{
		events:     make(chan PSIEvent, 32),
		done:       make(chan struct{}),
		cgroupRoot: cgroupRoot,
		thresholds: make(map[string]uint64),
		windowUs:   windowUs,
	}
}

// SetThreshold imposta la soglia per un tipo di pressure.
func (w *PSIWatcher) SetThreshold(typ string, stallUs uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.thresholds[typ] = stallUs
}

// Events restituisce il canale degli eventi.
func (w *PSIWatcher) Events() <-chan PSIEvent {
	return w.events
}

// Start avvia il monitoraggio dei pressure file.
func (w *PSIWatcher) Start() error {
	w.mu.Lock()
	types := make([]string, 0, len(w.thresholds))
	for t := range w.thresholds {
		types = append(types, t)
	}
	w.mu.Unlock()

	if len(types) == 0 {
		return fmt.Errorf("no pressure thresholds configured")
	}

	for _, typ := range types {
		w.wg.Add(1)
		go w.watch(typ)
	}

	return nil
}

// Stop ferma il monitoraggio.
func (w *PSIWatcher) Stop() {
	close(w.done)
	w.wg.Wait()
}

func (w *PSIWatcher) watch(typ string) {
	defer w.wg.Done()

	path := w.cgroupRoot + "/" + typ + ".pressure"

	for {
		select {
		case <-w.done:
			return
		default:
		}

		fd, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			select {
			case <-w.done:
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}

		w.mu.Lock()
		threshold := w.thresholds[typ]
		w.mu.Unlock()

		thresholdLine := fmt.Sprintf("some %d %d", threshold, w.windowUs)
		if _, err := fd.WriteString(thresholdLine); err != nil {
			fd.Close()
			select {
			case <-w.done:
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}

		pollFds := []unix.PollFd{
			{Fd: int32(fd.Fd()), Events: unix.POLLPRI},
		}

		for {
			_, err := unix.Poll(pollFds, -1)
			if err != nil {
				select {
				case <-w.done:
					fd.Close()
					return
				default:
				}
				break
			}

			if pollFds[0].Revents&unix.POLLPRI != 0 || pollFds[0].Revents&unix.POLLERR != 0 {
				data := make([]byte, 4096)
				n, err := fd.ReadAt(data, 0)
				if err != nil {
					break
				}

				stats, err := parsePSI(string(data[:n]))
				if err != nil {
					break
				}

				select {
				case w.events <- PSIEvent{
					Type:      typ,
					SomeAvg10: stats.SomeAvg10,
					Timestamp: time.Now(),
				}:
				case <-w.done:
					fd.Close()
					return
				default:
				}
			}
		}

		fd.Close()

		select {
		case <-w.done:
			return
		case <-time.After(5 * time.Second):
		}
	}
}
