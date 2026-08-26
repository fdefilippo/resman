package app

import (
	"context"
	"os"
	"sync"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/mcp"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

// App contains the daemon runtime components.
type App struct {
	cfg        *config.Config
	configPath string
	ctx        context.Context
	cancel     context.CancelFunc
	sigChan    <-chan os.Signal
	logger     *logging.Logger
	err        error
	cfgMu      sync.RWMutex
	psiMu      sync.RWMutex

	cgroupMgr          *cgroup.Manager
	metricsCollector   *metrics.Collector
	cpuSamplingCadence cpuSamplingCadenceSink
	dbManager          *database.DatabaseManager
	prometheusExporter *metrics.PrometheusExporter
	stateManager       *state.Manager
	configWatcher      *config.Watcher
	mcpServer          *mcp.Server
	psiWatcher         *cgroup.PSIWatcher
	psiEvents          <-chan cgroup.PSIEvent
	psiEventDriven     bool
	configReloaded     chan struct{}
}

func (a *App) currentConfig() *config.Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *App) setCurrentConfig(cfg *config.Config) {
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
}

// NewApp crea il builder dell'applicazione.
func NewApp(cfg *config.Config, configPath string, ctx context.Context, cancel context.CancelFunc, sigChan <-chan os.Signal, logger *logging.Logger) *App {
	return &App{
		cfg:            cfg,
		configPath:     configPath,
		ctx:            ctx,
		cancel:         cancel,
		sigChan:        sigChan,
		logger:         logger,
		configReloaded: make(chan struct{}, 1),
	}
}
