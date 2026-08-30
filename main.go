/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/app"
	"github.com/fdefilippo/resman/logging"
)

var version = "1.30.8"

const (
	exitStatusFailure       = 1
	exitStatusConfiguration = 78
)

func main() {
	// Parse command-line flags.
	configPath := flag.String("config", config.DefaultConfigPath, "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("resman %s\n", version)
		return
	}

	// Load and validate the initial configuration.
	cfg, err := config.LoadAndValidate(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration from %s: %v\n\n", *configPath, err)
		fmt.Fprintf(os.Stderr, "Common issues:\n")
		fmt.Fprintf(os.Stderr, "  - File does not exist: create %s from the example\n", config.DefaultConfigPath)
		fmt.Fprintf(os.Stderr, "  - Invalid syntax: check key=value format\n")
		fmt.Fprintf(os.Stderr, "  - Invalid values: verify thresholds, ports, and paths\n")
		os.Exit(exitStatusConfiguration)
	}

	// Initialize logging from the effective configuration.
	logging.InitLogger(cfg.LogLevel, cfg.LogFile, cfg.LogMaxSize, cfg.UseSyslog)
	logger := logging.GetLogger()

	logger.Info("Starting Resource Manager", "version", version)
	logger.Info("Configuration loaded successfully",
		"log_level", cfg.LogLevel,
		"log_file", cfg.LogFile,
		"use_syslog", cfg.UseSyslog,
	)

	if cfg.UseSyslog {
		logger.Info("Syslog logging enabled")
	} else {
		logger.Debug("File logging enabled")
	}

	// Set up graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Receive process lifecycle signals.
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Initialize application components.
	logger.Info("Initializing components:")

	err = app.NewApp(cfg, *configPath, ctx, cancel, sigChan, logger).
		WithCgroupManager().
		WithMetricsCollector().
		WithDatabase().
		WithPrometheus().
		WithStateManager().
		WithConfigWatcher().
		WithMCPServer().
		Run()
	if err != nil {
		os.Exit(applicationExitCode(err))
	}
}

func applicationExitCode(err error) int {
	if app.IsPermanentStartupError(err) {
		return exitStatusConfiguration
	}
	return exitStatusFailure
}
