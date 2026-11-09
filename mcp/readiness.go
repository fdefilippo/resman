package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/fdefilippo/resman/internal/readiness"
)

// SetReadinessProvider installs the application-owned in-memory status source.
func (s *Server) SetReadinessProvider(provider func() readiness.Snapshot) {
	s.mu.Lock()
	s.readinessProvider = provider
	s.mu.Unlock()
}

func (s *Server) readinessSnapshot() readiness.Snapshot {
	s.mu.RLock()
	provider := s.readinessProvider
	s.mu.RUnlock()
	if provider == nil {
		return readiness.Snapshot{Status: readiness.NotReady}
	}
	return provider()
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	snapshot := s.readinessSnapshot()
	s.writeReadiness(w, r, snapshot, struct {
		Status readiness.Status `json:"status"`
	}{snapshot.Status})
}

func (s *Server) handleReadinessDetails(w http.ResponseWriter, r *http.Request) {
	snapshot := s.readinessSnapshot()
	s.writeReadiness(w, r, snapshot, snapshot)
}

func (s *Server) writeReadiness(w http.ResponseWriter, r *http.Request, snapshot readiness.Snapshot, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status := http.StatusServiceUnavailable
	if snapshot.IsReady() {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			s.logger.Warn("Failed to write readiness response", "error", err)
		}
	}
}
