package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/fdefilippo/resman/config"
)

type limitHookEvent struct {
	UID             int       `json:"uid"`
	Username        string    `json:"username"`
	CPUUsage        float64   `json:"cpu_usage"`
	LimitedUsers    int       `json:"limited_users"`
	SharedCgroup    string    `json:"shared_cgroup"`
	Timestamp       time.Time `json:"timestamp"`
	ServerRole      string    `json:"server_role,omitempty"`
	LimitHookSource string    `json:"source"`
}

func (m *Manager) notifyUserLimited(cfg *config.Config, uid int, username string, metrics *SystemMetrics) {
	if cfg == nil || !cfg.LimitHookEnabled {
		return
	}

	m.mu.RLock()
	sharedCgroup := m.sharedCgroupPath
	m.mu.RUnlock()

	event := limitHookEvent{
		UID:             uid,
		Username:        username,
		CPUUsage:        metrics.UserCPUUsage[uid],
		LimitedUsers:    metrics.LimitedUsersCount,
		SharedCgroup:    sharedCgroup,
		Timestamp:       time.Now().UTC(),
		ServerRole:      cfg.ServerRole,
		LimitHookSource: "resman",
	}

	go m.runLimitHook(cfg, event)
}

func (m *Manager) runLimitHook(cfg *config.Config, event limitHookEvent) {
	timeout := time.Duration(cfg.LimitHookTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if cfg.LimitHookScript != "" {
		if err := runLimitHookScript(ctx, cfg.LimitHookScript, event); err != nil {
			m.logger.Warn("Limit hook script failed",
				"uid", event.UID,
				"username", event.Username,
				"script", cfg.LimitHookScript,
				"error", err,
			)
		}
	}

	if cfg.LimitHookURL != "" {
		if err := postLimitHook(ctx, cfg.LimitHookURL, event); err != nil {
			m.logger.Warn("Limit hook webservice failed",
				"uid", event.UID,
				"username", event.Username,
				"url", cfg.LimitHookURL,
				"error", err,
			)
		}
	}
}

func runLimitHookScript(ctx context.Context, script string, event limitHookEvent) error {
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(),
		"RESMAN_LIMIT_UID="+strconv.Itoa(event.UID),
		"RESMAN_LIMIT_USERNAME="+event.Username,
		"RESMAN_LIMIT_CPU_USAGE="+strconv.FormatFloat(event.CPUUsage, 'f', 2, 64),
		"RESMAN_LIMIT_LIMITED_USERS="+strconv.Itoa(event.LimitedUsers),
		"RESMAN_LIMIT_SHARED_CGROUP="+event.SharedCgroup,
		"RESMAN_LIMIT_TIMESTAMP="+event.Timestamp.Format(time.RFC3339),
		"RESMAN_LIMIT_SERVER_ROLE="+event.ServerRole,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}
	return nil
}

func postLimitHook(ctx context.Context, endpoint string, event limitHookEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal hook event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create hook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "resman-limit-hook")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post hook request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hook endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}
