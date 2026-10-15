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
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func newBufferLogger(level LogLevel) (*Logger, *bytes.Buffer) {
	var output bytes.Buffer
	return &Logger{
		state: &loggerState{
			level:  level,
			logger: log.New(&output, "", 0),
		},
		fields: make(map[string]interface{}),
	}, &output
}

type blockingLogWriter struct {
	started chan struct{}
	release chan struct{}
}

type injectedWriter struct {
	mu       sync.Mutex
	failures int
	err      error
	written  bytes.Buffer
}

func (w *injectedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failures != 0 {
		if w.failures > 0 {
			w.failures--
		}
		return 0, w.err
	}
	return w.written.Write(p)
}

type injectedSyslogSink struct {
	mu       sync.Mutex
	failures int
	writeErr error
	closeErr error
	writes   int
}

func (s *injectedSyslogSink) write(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.failures != 0 {
		if s.failures > 0 {
			s.failures--
		}
		return s.writeErr
	}
	return nil
}

func (s *injectedSyslogSink) Debug(message string) error   { return s.write(message) }
func (s *injectedSyslogSink) Info(message string) error    { return s.write(message) }
func (s *injectedSyslogSink) Warning(message string) error { return s.write(message) }
func (s *injectedSyslogSink) Err(message string) error     { return s.write(message) }
func (s *injectedSyslogSink) Close() error                 { return s.closeErr }

func newInjectedSyslogLogger(sink syslogSink, fallback io.Writer) *Logger {
	return &Logger{
		state: &loggerState{
			level:        DEBUG,
			useSyslog:    true,
			syslogWriter: sink,
			fallback:     fallback,
			health:       Health{Healthy: true},
		},
		fields: make(map[string]interface{}),
	}
}

func TestLoggerReturnsSyslogFailureAndRecoversHealth(t *testing.T) {
	sinkErr := errors.New("injected syslog write failure")
	sink := &injectedSyslogSink{failures: 1, writeErr: sinkErr}
	var fallback bytes.Buffer
	logger := newInjectedSyslogLogger(sink, &fallback)

	err := logger.InfoChecked("first record", "token", "not-retained-in-health")
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Info() error = %v, want injected syslog error", err)
	}
	health := logger.Health()
	if health.Healthy || health.TotalFailures != 1 || health.ConsecutiveFailures != 1 {
		t.Fatalf("Health() after failure = %+v", health)
	}
	if health.LastFailure.Sink != "syslog" || health.LastFailure.Operation != "write" {
		t.Fatalf("Health().LastFailure = %+v", health.LastFailure)
	}
	if strings.Contains(health.LastFailure.Error, "not-retained-in-health") {
		t.Fatalf("Health() retained log record content: %+v", health.LastFailure)
	}
	if !strings.Contains(fallback.String(), "Logging sink failure") {
		t.Fatalf("fallback output = %q", fallback.String())
	}
	for _, secret := range []string{"first record", "not-retained-in-health"} {
		if strings.Contains(fallback.String(), secret) {
			t.Fatalf("fallback retained original record content %q: %q", secret, fallback.String())
		}
	}

	if err := logger.InfoChecked("second record"); err != nil {
		t.Fatalf("Info() after transient failure error = %v", err)
	}
	health = logger.Health()
	if !health.Healthy || health.TotalFailures != 1 || health.ConsecutiveFailures != 0 {
		t.Fatalf("Health() after recovery = %+v", health)
	}
}

func TestLoggerReportsPersistentPrimaryAndFallbackFailures(t *testing.T) {
	primaryErr := errors.New(strings.Repeat("persistent syslog failure ", 40))
	fallbackErr := errors.New("persistent fallback failure")
	sink := &injectedSyslogSink{failures: -1, writeErr: primaryErr}
	fallback := &injectedWriter{failures: -1, err: fallbackErr}
	logger := newInjectedSyslogLogger(sink, fallback)

	for attempt := 1; attempt <= 2; attempt++ {
		err := logger.ErrorChecked("record that cannot be delivered")
		if !errors.Is(err, primaryErr) || !errors.Is(err, fallbackErr) {
			t.Fatalf("Error() attempt %d = %v, want primary and fallback failures", attempt, err)
		}
	}
	health := logger.Health()
	if health.Healthy || health.TotalFailures != 2 || health.ConsecutiveFailures != 2 {
		t.Fatalf("Health() after persistent failure = %+v", health)
	}
	if !strings.Contains(health.LastFailure.FallbackError, fallbackErr.Error()) {
		t.Fatalf("Health().LastFailure = %+v, want fallback error", health.LastFailure)
	}
	if got := len([]rune(health.LastFailure.Error)); got != maxFailureTextRunes {
		t.Fatalf("bounded primary failure length = %d, want %d", got, maxFailureTextRunes)
	}
}

func TestLoggerRecoversAFileSinkAfterTransientWriteFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "resman.log")
	logger, err := newFileLogger(INFO, logPath, 1024*1024, DefaultLogMaxBackups)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()

	writeErr := errors.New("injected file write failure")
	primary := &injectedWriter{failures: 1, err: writeErr}
	var fallback bytes.Buffer
	logger.state.logger.SetOutput(primary)
	logger.state.fallback = &fallback

	if err := logger.InfoChecked("first file record"); !errors.Is(err, writeErr) {
		t.Fatalf("Info() error = %v, want injected file error", err)
	}
	if logger.Health().Healthy {
		t.Fatal("Health() remained healthy after file write failure")
	}
	if err := logger.InfoChecked("record after reopen"); err != nil {
		t.Fatalf("Info() after reopen error = %v", err)
	}
	if !logger.Health().Healthy {
		t.Fatalf("Health() after file recovery = %+v", logger.Health())
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) error = %v", logPath, err)
	}
	if !strings.Contains(string(content), "record after reopen") {
		t.Fatalf("reopened log content = %q", content)
	}
}

func TestLoggerReportsEveryRotationOperationFailure(t *testing.T) {
	injectedErr := errors.New("injected rotation failure")
	tests := []struct {
		name      string
		operation string
		mutate    func(*loggerState)
	}{
		{
			name:      "stat",
			operation: "rotation_stat",
			mutate: func(state *loggerState) {
				state.fileOps.stat = func(*os.File) (os.FileInfo, error) { return nil, injectedErr }
			},
		},
		{
			name:      "close",
			operation: "rotation_close",
			mutate: func(state *loggerState) {
				state.fileOps.close = func(*os.File) error { return injectedErr }
			},
		},
		{
			name:      "remove",
			operation: "rotation_remove",
			mutate: func(state *loggerState) {
				state.fileOps.remove = func(string) error { return injectedErr }
			},
		},
		{
			name:      "rename",
			operation: "rotation_rename",
			mutate: func(state *loggerState) {
				state.fileOps.rename = func(string, string) error { return injectedErr }
			},
		},
		{
			name:      "reopen",
			operation: "rotation_reopen",
			mutate: func(state *loggerState) {
				state.fileOps.open = func(string, *logFileMetadata) (*os.File, error) { return nil, injectedErr }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "resman.log")
			logger, err := newFileLogger(INFO, logPath, 1, DefaultLogMaxBackups)
			if err != nil {
				t.Fatalf("newFileLogger() error = %v", err)
			}
			var fallback bytes.Buffer
			logger.state.fallback = &fallback
			logger.state.lastRotation = time.Now().Add(-2 * time.Second)
			tt.mutate(logger.state)

			err = logger.InfoChecked("record that triggers rotation")
			if !errors.Is(err, injectedErr) {
				t.Fatalf("Info() error = %v, want injected rotation error", err)
			}
			health := logger.Health()
			if health.Healthy || health.LastFailure.Operation != tt.operation {
				t.Fatalf("Health() = %+v, want operation %q", health, tt.operation)
			}
			if !strings.Contains(fallback.String(), tt.operation) {
				t.Fatalf("fallback output = %q, want operation %q", fallback.String(), tt.operation)
			}
			if strings.Contains(fallback.String(), "Log rotated due to size limit") {
				t.Fatalf("fallback claimed successful rotation: %q", fallback.String())
			}
			_ = logger.Close()
		})
	}
}

func TestLoggerRecoversRotationReopenWithOriginalMetadata(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "resman.log")
	logger, err := newFileLogger(INFO, logPath, 1, DefaultLogMaxBackups)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()

	reopenErr := errors.New("injected transient reopen failure")
	openCalls := 0
	metadataPreserved := false
	logger.state.fileOps.open = func(path string, metadata *logFileMetadata) (*os.File, error) {
		openCalls++
		if openCalls == 1 {
			return nil, reopenErr
		}
		metadataPreserved = metadata != nil
		return openSecureLogFile(path, metadata)
	}
	logger.state.lastRotation = time.Now().Add(-2 * time.Second)

	if err := logger.InfoChecked("record that triggers reopen failure"); !errors.Is(err, reopenErr) {
		t.Fatalf("Info() error = %v, want transient reopen failure", err)
	}
	if err := logger.InfoChecked("record after rotation recovery"); err != nil {
		t.Fatalf("Info() after rotation recovery error = %v", err)
	}
	if !metadataPreserved {
		t.Fatal("rotation recovery reopened without the original ownership and mode metadata")
	}
	if health := logger.Health(); !health.Healthy || health.TotalFailures != 1 {
		t.Fatalf("Health() after rotation recovery = %+v", health)
	}
}

func TestLoggerReportsSyslogCloseFailureWithoutRecursion(t *testing.T) {
	closeErr := errors.New("injected syslog close failure")
	sink := &injectedSyslogSink{closeErr: closeErr}
	var fallback bytes.Buffer
	logger := newInjectedSyslogLogger(sink, &fallback)

	if err := logger.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want injected close error", err)
	}
	if got := logger.Health().LastFailure.Operation; got != "close" {
		t.Fatalf("Health().LastFailure.Operation = %q, want close", got)
	}
	if sink.writes != 0 {
		t.Fatalf("failure reporting recursed through syslog %d times", sink.writes)
	}
	if !strings.Contains(fallback.String(), "Logging sink failure") {
		t.Fatalf("fallback output = %q", fallback.String())
	}
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func TestLoggerLevelRemainsAvailableWhileSinkWriteBlocks(t *testing.T) {
	writer := &blockingLogWriter{started: make(chan struct{}), release: make(chan struct{})}
	logger := &Logger{
		state:  &loggerState{level: INFO, logger: log.New(writer, "", 0)},
		fields: make(map[string]interface{}),
	}
	writeDone := make(chan struct{})
	go func() {
		logger.Info("blocked write")
		close(writeDone)
	}()
	<-writer.started

	levelDone := make(chan struct{})
	go func() {
		logger.SetLevel("DEBUG")
		close(levelDone)
	}()
	select {
	case <-levelDone:
	case <-time.After(time.Second):
		close(writer.release)
		<-writeDone
		t.Fatal("SetLevel() blocked behind log sink I/O")
	}
	close(writer.release)
	<-writeDone
}

func TestNewFileLogger(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "test.log")
	logger, err := newFileLogger(INFO, logFile, 1024*1024, DefaultLogMaxBackups)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()

	if logger.state.level != INFO {
		t.Errorf("Logger level: got %v, expected INFO", logger.state.level)
	}
	if logger.state.filePath != logFile {
		t.Errorf("Logger filePath: got %q, expected %q", logger.state.filePath, logFile)
	}
	if logger.state.maxSize != 1024*1024 {
		t.Errorf("Logger maxSize: got %d, expected %d", logger.state.maxSize, 1024*1024)
	}
	if logger.state.file == nil {
		t.Fatal("newFileLogger() did not open the log file")
	}
	assertLogFileMode(t, logFile, 0600)
}

func TestNewFileLoggerRestrictsExistingFileAccess(t *testing.T) {
	tests := []struct {
		name     string
		source   os.FileMode
		expected os.FileMode
	}{
		{name: "world readable", source: 0644, expected: 0640},
		{name: "world writable and executable", source: 0777, expected: 0660},
		{name: "intentional group read", source: 0640, expected: 0640},
		{name: "owner only", source: 0600, expected: 0600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logFile := filepath.Join(t.TempDir(), "test.log")
			if err := os.WriteFile(logFile, []byte("existing\n"), tt.source); err != nil {
				t.Fatalf("write existing log: %v", err)
			}
			if err := os.Chmod(logFile, tt.source); err != nil {
				t.Fatalf("chmod existing log: %v", err)
			}

			logger, err := newFileLogger(INFO, logFile, 1024*1024, DefaultLogMaxBackups)
			if err != nil {
				t.Fatalf("newFileLogger() error = %v", err)
			}
			defer func() { _ = logger.Close() }()
			assertLogFileMode(t, logFile, tt.expected)
		})
	}
}

func TestNewFileLoggerRejectsSymbolicLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	if err := os.WriteFile(target, []byte("existing\n"), 0644); err != nil {
		t.Fatalf("write target log: %v", err)
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatalf("chmod target log: %v", err)
	}
	link := filepath.Join(dir, "resman.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create log symlink: %v", err)
	}

	logger, err := newFileLogger(INFO, link, 1024*1024, DefaultLogMaxBackups)
	if err == nil || logger != nil {
		t.Fatalf("newFileLogger(symlink) = (%v, %v), want nil logger and error", logger, err)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("newFileLogger(symlink) error = %q, want explicit symlink refusal", err)
	}
	assertLogFileMode(t, target, 0644)
}

func TestNewFileLoggerRejectsNonPositiveMaxSize(t *testing.T) {
	for _, maxSize := range []int64{0, -1} {
		if logger, err := newFileLogger(INFO, filepath.Join(t.TempDir(), "test.log"), maxSize, DefaultLogMaxBackups); err == nil || logger != nil {
			t.Fatalf("newFileLogger(maxSize=%d) = (%v, %v), want nil logger and error", maxSize, logger, err)
		}
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
	if logger.state.level != DEBUG {
		t.Errorf("SetLevel: got %v, expected DEBUG", logger.state.level)
	}

	logger.SetLevel("ERROR")
	if logger.state.level != ERROR {
		t.Errorf("SetLevel: got %v, expected ERROR", logger.state.level)
	}
}

func TestLoggerClose(t *testing.T) {
	logger, err := newFileLogger(INFO, filepath.Join(t.TempDir(), "test.log"), 1024*1024, DefaultLogMaxBackups)
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
	if err := os.WriteFile(logFile, []byte("existing log\n"), 0644); err != nil {
		t.Fatalf("write existing log: %v", err)
	}
	if err := os.Chmod(logFile, 0644); err != nil {
		t.Fatalf("chmod existing log: %v", err)
	}
	logger, err := newFileLogger(INFO, logFile, 1, DefaultLogMaxBackups)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	logger.state.lastRotation = time.Now().Add(-2 * time.Second)

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
	assertLogFileMode(t, logFile, 0640)
	assertLogFileMode(t, logFile+".1", 0640)
}

func assertLogFileMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != expected {
		t.Fatalf("%s mode = %04o, want %04o", path, got, expected)
	}
}

func TestSetLevelConcurrentWithLogging(t *testing.T) {
	logger := &Logger{
		state: &loggerState{
			level:  INFO,
			logger: log.New(io.Discard, "", 0),
		},
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

func TestLoggerSanitizesControlCharacters(t *testing.T) {
	logger, output := newBufferLogger(INFO)
	logger.WithField("user\nname", "alice\radmin").Info(
		"accepted\n[ERROR] forged",
		"process\tname",
		"worker\x00hidden",
	)

	text := output.String()
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("logger emitted more than one record: %q", text)
	}
	if strings.ContainsAny(strings.TrimSuffix(text, "\n"), "\r\t\x00") {
		t.Fatalf("logger emitted raw control characters: %q", text)
	}
	for _, want := range []string{
		`accepted\n[ERROR] forged`,
		`user\nname=alice\radmin`,
		`process\tname=worker\x00hidden`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("logger output does not contain escaped value %q: %q", want, text)
		}
	}
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

func TestWithFieldSharesLevelAndRotationState(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "test.log")
	logger, err := newFileLogger(INFO, logFile, 1, DefaultLogMaxBackups)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()

	child := logger.WithField("uid", 1000)
	if child.state != logger.state {
		t.Fatal("WithField() did not preserve shared logger state")
	}

	child.SetLevel("ERROR")
	logger.Info("suppressed through shared level")
	child.SetLevel("INFO")

	logger.state.lastRotation = time.Now().Add(-2 * time.Second)
	child.Info("message that rotates through child logger")
	logger.Info("parent writes after child rotation")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read active log after child rotation: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "suppressed through shared level") {
		t.Fatalf("parent did not observe child level update: %q", text)
	}
	if !strings.Contains(text, "parent writes after child rotation") {
		t.Fatalf("parent retained stale writer after child rotation: %q", text)
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
	if got := logger.state.logger.Writer(); got != os.Stderr {
		t.Errorf("fallback writer = %v, want os.Stderr", got)
	}
}

// rotateOnce writes one record that crosses LOG_MAX_SIZE immediately.
func rotateOnce(t *testing.T, logger *Logger, record string) {
	t.Helper()
	logger.state.lastRotation = time.Now().Add(-2 * time.Second)
	if err := logger.InfoChecked(record); err != nil {
		t.Fatalf("InfoChecked(%q) error = %v", record, err)
	}
}

func readRotatedLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.HasSuffix(path, ".gz") {
		return string(data)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open gzip %s: %v", path, err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress %s: %v", path, err)
	}
	return string(plain)
}

// TestRotationKeepsABoundedSetOfBackups pins resman-8bz: ResMan is the only
// rotator of its file. The newest copy is LOG_FILE.1, older copies are
// LOG_FILE.2.gz up to LOG_FILE.<n>.gz in age order, and nothing older survives,
// so a busy day can no longer destroy history by overwriting a single .1.
func TestRotationKeepsABoundedSetOfBackups(t *testing.T) {
	tests := []struct {
		name       string
		maxBackups int
		rotations  int
		want       map[string]string // backup suffix -> record it must contain
		absent     []string
	}{
		{
			name: "three copies after five rotations", maxBackups: 3, rotations: 5,
			want:   map[string]string{".1": "record 5", ".2.gz": "record 4", ".3.gz": "record 3"},
			absent: []string{".4.gz", ".2", ".3", ".1.gz"},
		},
		{
			name: "single copy is never compressed", maxBackups: 1, rotations: 3,
			want:   map[string]string{".1": "record 3"},
			absent: []string{".2.gz", ".1.gz"},
		},
		{
			name: "fewer rotations than copies", maxBackups: 7, rotations: 2,
			want:   map[string]string{".1": "record 2", ".2.gz": "record 1"},
			absent: []string{".3.gz"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logFile := filepath.Join(t.TempDir(), "resman.log")
			logger, err := newFileLogger(INFO, logFile, 1, tt.maxBackups)
			if err != nil {
				t.Fatalf("newFileLogger() error = %v", err)
			}
			defer func() { _ = logger.Close() }()
			for index := 1; index <= tt.rotations; index++ {
				rotateOnce(t, logger, fmt.Sprintf("record %d", index))
			}
			for suffix, record := range tt.want {
				content := readRotatedLog(t, logFile+suffix)
				if !strings.Contains(content, record) {
					t.Fatalf("%s = %q, want %q", suffix, content, record)
				}
				for other := 1; other <= tt.rotations; other++ {
					if name := fmt.Sprintf("record %d", other); name != record && strings.Contains(content, name+"\n") {
						t.Fatalf("%s mixes %q into the copy of %q", suffix, name, record)
					}
				}
				assertLogFileMode(t, logFile+suffix, 0600)
			}
			for _, suffix := range tt.absent {
				if _, err := os.Lstat(logFile + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s exists (err %v), want removed or never created", suffix, err)
				}
			}
			matches, err := filepath.Glob(logFile + ".*.tmp")
			if err != nil || len(matches) != 0 {
				t.Fatalf("temporary compression files left behind: %v (err %v)", matches, err)
			}
		})
	}
}

func TestRotationPrunesCopiesAboveTheLimitOnly(t *testing.T) {
	directory := t.TempDir()
	logFile := filepath.Join(directory, "resman.log")
	foreign := []string{"resman.log-20261015", "resman.log-20261014.gz", "resman.log.notes", "resman.log.01.gz", "other.log.9.gz"}
	stale := []string{"resman.log.5.gz", "resman.log.9.gz", "resman.log.12"}
	for _, name := range append(append([]string{}, foreign...), stale...) {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	logger, err := newFileLogger(INFO, logFile, 1, 3)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()
	rotateOnce(t, logger, "record after lowering LOG_MAX_BACKUPS")

	for _, name := range stale {
		if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived above LOG_MAX_BACKUPS=3 (err %v)", name, err)
		}
	}
	for _, name := range foreign {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("%s was removed, want files outside the rotation pattern untouched: %v", name, err)
		}
	}
}

func TestRotationCompressionFailurePreservesTheNewestCopy(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "resman.log")
	if err := os.WriteFile(logFile+".1", []byte("previous copy\n"), 0600); err != nil {
		t.Fatalf("write previous copy: %v", err)
	}
	logger, err := newFileLogger(INFO, logFile, 1, 3)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()
	var fallback bytes.Buffer
	logger.state.fallback = &fallback
	injectedErr := errors.New("injected compression failure")
	logger.state.fileOps.compress = func(string, string, logFileMetadata) error { return injectedErr }
	logger.state.lastRotation = time.Now().Add(-2 * time.Second)

	if err := logger.InfoChecked("record that triggers rotation"); !errors.Is(err, injectedErr) {
		t.Fatalf("InfoChecked() error = %v, want injected compression failure", err)
	}
	if health := logger.Health(); health.Healthy || health.LastFailure.Operation != "rotation_compress" {
		t.Fatalf("Health() = %+v, want rotation_compress", health)
	}
	if content := readRotatedLog(t, logFile+".1"); content != "previous copy\n" {
		t.Fatalf("newest copy = %q, want it untouched by a failed rotation", content)
	}
	if err := logger.InfoChecked("record after failed rotation"); err != nil && !errors.Is(err, injectedErr) {
		t.Fatalf("active log is not writable after a failed rotation: %v", err)
	}
	if content := readRotatedLog(t, logFile); !strings.Contains(content, "record after failed rotation") {
		t.Fatalf("active log = %q, want later records kept", content)
	}
}

func TestCompressLogBackupRefusesSymlinks(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("not a log"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	source := filepath.Join(directory, "resman.log.1")
	if err := os.Symlink(target, source); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := compressLogBackup(source, filepath.Join(directory, "resman.log.2.gz"), logFileMetadata{mode: 0600})
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("compressLogBackup(symlink) error = %v, want ELOOP", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "resman.log.2.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("compressed copy created from a symlink (err %v)", err)
	}
}

func TestNewFileLoggerRejectsMissingBackupCount(t *testing.T) {
	for _, maxBackups := range []int{0, -1} {
		if logger, err := newFileLogger(INFO, filepath.Join(t.TempDir(), "test.log"), 1024, maxBackups); err == nil || logger != nil {
			t.Fatalf("newFileLogger(maxBackups=%d) = (%v, %v), want nil logger and error", maxBackups, logger, err)
		}
	}
}
