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

type appLogger interface {
	Debug(string, ...interface{})
	Info(string, ...interface{})
	Warn(string, ...interface{})
	Error(string, ...interface{})
	InfoChecked(string, ...interface{}) error
}

type configWatcher interface {
	Reload(context.Context) error
	ForceReload(context.Context) error
	Stop() error
}

// App contains the daemon runtime components.
type App struct {
	cfg        *config.Config
	configPath string
	ctx        context.Context
	cancel     context.CancelFunc
	sigChan    <-chan os.Signal
	logger     appLogger
	err        error
	cfgMu      sync.RWMutex
	psiMu      sync.RWMutex

	cgroupMgr          *cgroup.Manager
	metricsCollector   *metrics.Collector
	cpuSamplingCadence cpuSamplingCadenceSink
	dbManager          *database.DatabaseManager
	prometheusExporter *metrics.PrometheusExporter
	stateManager       *state.Manager
	configWatcher      configWatcher
	mcpServer          *mcp.Server
	psiWatcher         *cgroup.PSIWatcher
	psiEvents          <-chan cgroup.PSIEvent
	psiEventDriven     bool
	configReloaded     chan struct{}
	notifyReady        func() error
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

// NewApp constructs the daemon application from its runtime dependencies.
func NewApp(cfg *config.Config, configPath string, ctx context.Context, cancel context.CancelFunc, sigChan <-chan os.Signal, logger *logging.Logger) *App {
	return &App{
		cfg:            cfg,
		configPath:     configPath,
		ctx:            ctx,
		cancel:         cancel,
		sigChan:        sigChan,
		logger:         logger,
		configReloaded: make(chan struct{}, 1),
		notifyReady:    notifySystemdReady,
	}
}
