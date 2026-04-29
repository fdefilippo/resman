package app

import (
	"context"
	"os"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/mcp"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

// App contiene i componenti runtime del daemon.
type App struct {
	cfg        *config.Config
	configPath string
	ctx        context.Context
	cancel     context.CancelFunc
	sigChan    <-chan os.Signal
	logger     *logging.Logger
	err        error

	cgroupMgr          *cgroup.Manager
	metricsCollector   *metrics.Collector
	dbManager          *database.DatabaseManager
	prometheusExporter *metrics.PrometheusExporter
	stateManager       *state.Manager
	configWatcher      *config.Watcher
	mcpServer          *mcp.Server
	psiWatcher         *cgroup.PSIWatcher
	psiEvents          <-chan cgroup.PSIEvent
	psiEventDriven     bool
}

// NewApp crea il builder dell'applicazione.
func NewApp(cfg *config.Config, configPath string, ctx context.Context, cancel context.CancelFunc, sigChan <-chan os.Signal, logger *logging.Logger) *App {
	return &App{
		cfg:        cfg,
		configPath: configPath,
		ctx:        ctx,
		cancel:     cancel,
		sigChan:    sigChan,
		logger:     logger,
	}
}
