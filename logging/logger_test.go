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
package logging

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newBufferLogger(level LogLevel) (*Logger, *bytes.Buffer) {
	var output bytes.Buffer
	return &Logger{
		level:  level,
		logger: log.New(&output, "", 0),
		fields: make(map[string]interface{}),
	}, &output
}

func TestNewFileLogger(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "test.log")
	logger, err := newFileLogger(INFO, logFile, 1024*1024)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()

	if logger.level != INFO {
		t.Errorf("Logger level: got %v, expected INFO", logger.level)
	}
	if logger.filePath != logFile {
		t.Errorf("Logger filePath: got %q, expected %q", logger.filePath, logFile)
	}
	if logger.maxSize != 1024*1024 {
		t.Errorf("Logger maxSize: got %d, expected %d", logger.maxSize, 1024*1024)
	}
	if logger.file == nil {
		t.Fatal("newFileLogger() did not open the log file")
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		expected LogLevel
	}{
		{"DEBUG", "DEBUG", DEBUG},
		{"INFO", "INFO", INFO},
		{"WARN", "WARN", WARN},
		{"ERROR", "ERROR", ERROR},
		{"INVALID", "INVALID", INFO}, // Default to INFO
		{"debug", "debug", INFO},     // Case sensitive, defaults to INFO
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogLevel(tt.level)
			if got != tt.expected {
				t.Errorf("parseLogLevel(%q): got %v, expected %v", tt.level, got, tt.expected)
			}
		})
	}
}

func TestLoggerMethods(t *testing.T) {
	logger, output := newBufferLogger(DEBUG)

	logger.Debug("Debug message", "key", "value")
	logger.Info("Info message", "key", "value")
	logger.Warn("Warning message", "key", "value")
	logger.Error("Error message", "key", "value")

	for _, want := range []string{"[DEBUG] Debug message", "[INFO] Info message", "[WARN] Warning message", "[ERROR] Error message", "key=value"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("logger output does not contain %q: %s", want, output.String())
		}
	}
}

func TestLoggerLevelFiltering(t *testing.T) {
	logger, output := newBufferLogger(INFO)
	logger.Debug("suppressed")
	logger.Info("visible")

	if strings.Contains(output.String(), "suppressed") {
		t.Errorf("DEBUG message was not filtered: %s", output.String())
	}
	if !strings.Contains(output.String(), "visible") {
		t.Errorf("INFO message was filtered: %s", output.String())
	}
}

func TestSetLevel(t *testing.T) {
	logger, _ := newBufferLogger(INFO)

	logger.SetLevel("DEBUG")
	if logger.level != DEBUG {
		t.Errorf("SetLevel: got %v, expected DEBUG", logger.level)
	}

	logger.SetLevel("ERROR")
	if logger.level != ERROR {
		t.Errorf("SetLevel: got %v, expected ERROR", logger.level)
	}
}

func TestLoggerClose(t *testing.T) {
	logger, err := newFileLogger(INFO, filepath.Join(t.TempDir(), "test.log"), 1024*1024)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}

	if err := logger.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

func TestLogRotation(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")
	logger, err := newFileLogger(INFO, logFile, 1)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	logger.lastRotation = time.Now().Add(-2 * time.Second)

	logged := make(chan struct{})
	go func() {
		logger.Info("message that forces deterministic rotation")
		close(logged)
	}()

	select {
	case <-logged:
	case <-time.After(time.Second):
		t.Fatal("logging deadlocked during successful rotation")
	}
	defer func() { _ = logger.Close() }()

	if _, err := os.Stat(logFile + ".1"); err != nil {
		t.Fatalf("rotated log file not found: %v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read active log after rotation: %v", err)
	}
	if !strings.Contains(string(data), "Log rotated due to size limit") {
		t.Fatalf("active log does not contain rotation record: %q", data)
	}
}

func TestSetLevelConcurrentWithLogging(t *testing.T) {
	logger := &Logger{
		level:  INFO,
		logger: log.New(io.Discard, "", 0),
		fields: make(map[string]interface{}),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			logger.SetLevel("DEBUG")
			logger.SetLevel("ERROR")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			logger.Debug("concurrent debug message")
			logger.Error("concurrent error message")
		}
	}()
	wg.Wait()
}

func TestLoggerWithMultipleFields(t *testing.T) {
	logger, output := newBufferLogger(INFO)
	logger.WithField("uid", 1000).WithField("username", "test-user").Info("limited")

	for _, want := range []string{"uid=1000", "username=test-user", "limited"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("logger output does not contain %q: %s", want, output.String())
		}
	}
}

func TestLoggerTimestamp(t *testing.T) {
	logger, output := newBufferLogger(INFO)
	logger.Info("timestamped")

	text := output.String()
	if len(text) < 20 {
		t.Fatalf("logger output is too short: %q", text)
	}
	if _, err := time.Parse("2006-01-02 15:04:05", text[1:20]); err != nil {
		t.Errorf("logger timestamp %q is invalid: %v", text[1:20], err)
	}
}

func TestLoggerFallbackUsesStderr(t *testing.T) {
	logger := createStderrLogger(INFO)
	if got := logger.logger.Writer(); got != os.Stderr {
		t.Errorf("fallback writer = %v, want os.Stderr", got)
	}
}
