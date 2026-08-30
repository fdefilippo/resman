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
// logging/logger.go
package logging

import (
	"errors"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/fdefilippo/resman/internal/operationgate"
)

// LogLevel identifies a supported logging severity.
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR

	defaultLogFileMode   = os.FileMode(0600)
	permittedLogFileMode = os.FileMode(0660)
)

var (
	// levelNames maps logging levels to their wire representation.
	levelNames = map[LogLevel]string{
		DEBUG: "DEBUG",
		INFO:  "INFO",
		WARN:  "WARN",
		ERROR: "ERROR",
	}

	// currentLogger is the process-wide logger singleton.
	currentLogger *Logger
	once          sync.Once

	defaultFallbackWriter io.Writer = os.Stderr
)

// Logger writes structured records to one ordered sink with rotation support.
type Logger struct {
	state  *loggerState
	fields map[string]interface{}
}

type loggerState struct {
	mu           sync.RWMutex
	healthMu     sync.RWMutex
	writeGate    operationgate.Gate
	level        LogLevel
	file         *os.File
	filePath     string
	maxSize      int64
	logger       *log.Logger
	lastRotation time.Time
	useSyslog    bool
	syslogWriter syslogSink
	fallback     io.Writer
	fileOps      logFileOperations
	reopenMeta   *logFileMetadata
	health       Health
}

type syslogSink interface {
	Debug(string) error
	Info(string) error
	Warning(string) error
	Err(string) error
	Close() error
}

type logFileOperations struct {
	stat   func(*os.File) (os.FileInfo, error)
	close  func(*os.File) error
	remove func(string) error
	rename func(string, string) error
	open   func(string, *logFileMetadata) (*os.File, error)
}

type logFileMetadata struct {
	mode              os.FileMode
	uid               int
	gid               int
	preserveOwnership bool
}

func defaultLogFileOperations() logFileOperations {
	return logFileOperations{
		stat:  func(file *os.File) (os.FileInfo, error) { return file.Stat() },
		close: func(file *os.File) error { return file.Close() },
		remove: func(path string) error {
			return os.Remove(path)
		},
		rename: os.Rename,
		open:   openSecureLogFile,
	}
}

func (s *loggerState) operations() logFileOperations {
	defaults := defaultLogFileOperations()
	operations := s.fileOps
	if operations.stat == nil {
		operations.stat = defaults.stat
	}
	if operations.close == nil {
		operations.close = defaults.close
	}
	if operations.remove == nil {
		operations.remove = defaults.remove
	}
	if operations.rename == nil {
		operations.rename = defaults.rename
	}
	if operations.open == nil {
		operations.open = defaults.open
	}
	return operations
}

// InitLogger initializes the process-wide logger once.
func InitLogger(level string, filePath string, maxSize int, useSyslog bool) {
	once.Do(func() {
		logLevel := parseLogLevel(level)

		// Syslog construction failure degrades directly to stderr.
		if useSyslog {
			syslogWriter, err := syslog.New(syslog.LOG_DAEMON|syslog.LOG_INFO, "resman")
			if err != nil {
				log.Printf("ERROR: Failed to initialize syslog: %v", err)
				// Fallback to stderr so protocol streams on stdout remain clean.
				currentLogger = createStderrLogger(logLevel)
				return
			}

			currentLogger = &Logger{
				state: &loggerState{
					level:        logLevel,
					logger:       log.New(syslogWriter, "", 0),
					useSyslog:    true,
					syslogWriter: syslogWriter,
					fallback:     os.Stderr,
					health:       Health{Healthy: true},
				},
				fields: make(map[string]interface{}),
			}

			_ = currentLogger.logInternal(INFO, "Logger initialized (syslog)",
				"level", levelNames[logLevel],
				"syslog", true,
			)
			return
		}

		fileLogger, err := newFileLogger(logLevel, filePath, int64(maxSize))
		if err != nil {
			log.Printf("ERROR: Failed to initialize file logger: %v", err)
			// Fallback to stderr so protocol streams on stdout remain clean.
			currentLogger = createStderrLogger(logLevel)
			return
		}
		currentLogger = fileLogger

		_ = currentLogger.logInternal(INFO, "Logger initialized",
			"level", levelNames[logLevel],
			"file", filePath,
			"max_size", fmt.Sprintf("%d bytes", maxSize),
		)
	})
}

func newFileLogger(level LogLevel, filePath string, maxSize int64) (*Logger, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("log max size must be greater than 0, got %d", maxSize)
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory %s: %w", filepath.Dir(filePath), err)
	}

	file, err := openSecureLogFile(filePath, nil)
	if err != nil {
		return nil, err
	}

	return &Logger{
		state: &loggerState{
			level:        level,
			file:         file,
			filePath:     filePath,
			maxSize:      maxSize,
			logger:       log.New(file, "", 0),
			lastRotation: time.Now(),
			fallback:     os.Stderr,
			health:       Health{Healthy: true},
		},
		fields: make(map[string]interface{}),
	}, nil
}

// openSecureLogFile opens a log without ever creating it more broadly than 0600.
// Existing files preserve intentional owner/group read-write access while
// removing execute and all access for other users. Symlinks and non-regular
// destinations are rejected so permission changes apply to the managed inode.
func openSecureLogFile(filePath string, metadata *logFileMetadata) (*os.File, error) {
	if info, err := os.Lstat(filePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symbolic link log file %s", filePath)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to inspect log path %s: %w", filePath, err)
	}

	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	file, err := os.OpenFile(filePath, flags, defaultLogFileMode)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", filePath, err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to inspect log file %s: %w", filePath, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("log path %s must be a regular file", filePath)
	}

	if metadata == nil {
		metadata = &logFileMetadata{mode: restrictiveLogFileMode(info.Mode())}
	}
	if metadata.preserveOwnership {
		if err := file.Chown(metadata.uid, metadata.gid); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf(
				"failed to preserve log ownership %d:%d for %s: %w",
				metadata.uid,
				metadata.gid,
				filePath,
				err,
			)
		}
	}
	if err := file.Chmod(metadata.mode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to secure log file %s with mode %04o: %w", filePath, metadata.mode, err)
	}
	return file, nil
}

func restrictiveLogFileMode(mode os.FileMode) os.FileMode {
	return mode.Perm() & permittedLogFileMode
}

func metadataForLogRotation(info os.FileInfo) logFileMetadata {
	metadata := logFileMetadata{mode: restrictiveLogFileMode(info.Mode())}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		metadata.uid = int(stat.Uid)
		metadata.gid = int(stat.Gid)
		metadata.preserveOwnership = true
	}
	return metadata
}

// GetLogger returns the initialized process-wide logger.
func GetLogger() *Logger {
	if currentLogger == nil {
		// If uninitialized, create the default logger with stderr fallback.
		InitLogger("INFO", "/var/log/resman.log", 10*1024*1024, false)
	}
	return currentLogger
}

// parseLogLevel converts a configuration value to a LogLevel.
func parseLogLevel(level string) LogLevel {
	switch level {
	case "DEBUG":
		return DEBUG
	case "WARN":
		return WARN
	case "ERROR":
		return ERROR
	default:
		return INFO
	}
}

// createStderrLogger creates a fallback logger that cannot corrupt stdout protocols.
func createStderrLogger(level LogLevel) *Logger {
	return &Logger{
		state: &loggerState{
			level:    level,
			logger:   log.New(os.Stderr, "", 0),
			fallback: os.Stderr,
			health:   Health{Healthy: true},
		},
		fields: make(map[string]interface{}),
	}
}

// logInternal formats and writes one ordered log record.
func (l *Logger) logInternal(level LogLevel, msg string, keyvals ...interface{}) error {
	l.state.mu.RLock()
	configuredLevel := l.state.level
	l.state.mu.RUnlock()
	if level < configuredLevel {
		return nil
	}

	logMsg := l.formatMessage(level, msg, keyvals...)
	leaveWrite := l.state.writeGate.Enter()
	defer leaveWrite()

	operation, err := l.writePrimaryRecord(level, logMsg)
	if err != nil {
		return l.reportSinkFailure(operation, err)
	}

	if l.state.file != nil {
		operation, err = l.checkAndRotate()
		if err != nil {
			return l.reportSinkFailure(operation, err)
		}
	}

	l.recordSinkSuccess()
	return nil
}

func (l *Logger) writePrimaryRecord(level LogLevel, logMsg string) (string, error) {
	if l.state.useSyslog {
		if l.state.syslogWriter == nil {
			return "write", fmt.Errorf("syslog writer is unavailable")
		}
		switch level {
		case DEBUG:
			return "write", l.state.syslogWriter.Debug(logMsg)
		case INFO:
			return "write", l.state.syslogWriter.Info(logMsg)
		case WARN:
			return "write", l.state.syslogWriter.Warning(logMsg)
		case ERROR:
			return "write", l.state.syslogWriter.Err(logMsg)
		default:
			return "write", l.state.syslogWriter.Info(logMsg)
		}
	}

	if l.state.filePath != "" && l.state.file == nil {
		if err := l.reopenActiveLog(); err != nil {
			return "reopen", err
		}
	}
	if l.state.logger == nil {
		return "write", fmt.Errorf("log writer is unavailable")
	}
	if err := l.state.logger.Output(2, logMsg); err != nil {
		if l.state.file != nil {
			closeErr := l.state.operations().close(l.state.file)
			if closeErr == nil {
				l.state.file = nil
			}
			return "write", errors.Join(err, closeErr)
		}
		return "write", err
	}
	return "write", nil
}

func (l *Logger) formatMessage(level LogLevel, msg string, keyvals ...interface{}) string {
	// Prefix every record with its timestamp and severity.
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMsg := fmt.Sprintf("[%s] [%s] %s", timestamp, levelNames[level], sanitizeLogValue(msg))

	// Add immutable contextual fields copied by WithField.
	for k, v := range l.fields {
		logMsg += fmt.Sprintf(" %s=%s", sanitizeLogValue(k), sanitizeLogValue(v))
	}

	// Append per-record key-value pairs.
	if len(keyvals) > 0 {
		for i := 0; i < len(keyvals); i += 2 {
			if i+1 < len(keyvals) {
				logMsg += fmt.Sprintf(" %s=%s", sanitizeLogValue(keyvals[i]), sanitizeLogValue(keyvals[i+1]))
			} else {
				logMsg += fmt.Sprintf(" %s=", sanitizeLogValue(keyvals[i]))
			}
		}
	}
	return logMsg
}

func sanitizeLogValue(value interface{}) string {
	quoted := strconv.Quote(fmt.Sprint(value))
	return quoted[1 : len(quoted)-1]
}

// checkAndRotate rotates an oversized file while the write gate is held.
func (l *Logger) checkAndRotate() (string, error) {
	// Limit filesystem metadata checks to one per second.
	if time.Since(l.state.lastRotation) < time.Second {
		return "", nil
	}

	l.state.lastRotation = time.Now()

	info, err := l.state.operations().stat(l.state.file)
	if err != nil {
		return "rotation_stat", fmt.Errorf("inspect active log before rotation: %w", err)
	}

	if info.Size() > l.state.maxSize {
		return l.rotateLog()
	}
	return "", nil
}

// rotateLog rotates the log while preserving its restrictive owner/group access.
func (l *Logger) rotateLog() (string, error) {
	operations := l.state.operations()
	info, err := operations.stat(l.state.file)
	if err != nil {
		return "rotation_stat", fmt.Errorf("inspect active log metadata before rotation: %w", err)
	}
	metadata := metadataForLogRotation(info)

	if err := operations.close(l.state.file); err != nil {
		// A failed Close has ambiguous descriptor state. Keep the existing handle
		// so the next write can prove whether it remains usable instead of
		// opening a second descriptor and potentially leaking the first one.
		return "rotation_close", fmt.Errorf("close active log before rotation: %w", err)
	}
	l.state.file = nil

	backupPath := l.state.filePath + ".1"
	if err := operations.remove(backupPath); err != nil && !os.IsNotExist(err) {
		return "rotation_remove", errors.Join(
			fmt.Errorf("remove previous rotated log %s: %w", backupPath, err),
			l.reopenActiveLog(),
		)
	}

	if err := operations.rename(l.state.filePath, backupPath); err != nil {
		return "rotation_rename", errors.Join(
			fmt.Errorf("rename active log to %s: %w", backupPath, err),
			l.reopenActiveLog(),
		)
	}

	file, err := operations.open(l.state.filePath, &metadata)
	if err != nil {
		l.state.reopenMeta = &metadata
		return "rotation_reopen", fmt.Errorf("reopen active log after rotation: %w", err)
	}

	l.state.file = file
	l.state.reopenMeta = nil
	if l.state.logger == nil {
		l.state.logger = log.New(file, "", 0)
	} else {
		l.state.logger.SetOutput(file)
	}

	l.state.mu.RLock()
	configuredLevel := l.state.level
	l.state.mu.RUnlock()
	if INFO >= configuredLevel {
		if err := l.state.logger.Output(2, l.formatMessage(INFO, "Log rotated due to size limit")); err != nil {
			closeErr := operations.close(file)
			l.state.file = nil
			return "rotation_notice", errors.Join(err, closeErr)
		}
	}
	return "", nil
}

func (l *Logger) reopenActiveLog() error {
	if l.state.filePath == "" {
		return fmt.Errorf("active log path is unavailable")
	}
	file, err := l.state.operations().open(l.state.filePath, l.state.reopenMeta)
	if err != nil {
		return fmt.Errorf("reopen active log %s: %w", l.state.filePath, err)
	}
	l.state.file = file
	l.state.reopenMeta = nil
	if l.state.logger == nil {
		l.state.logger = log.New(file, "", 0)
	} else {
		l.state.logger.SetOutput(file)
	}
	return nil
}

// Debug writes a best-effort DEBUG record and publishes failures through Health.
func (l *Logger) Debug(msg string, keyvals ...interface{}) {
	_ = l.DebugChecked(msg, keyvals...)
}

// DebugChecked writes a DEBUG record and returns any configured-sink failure.
func (l *Logger) DebugChecked(msg string, keyvals ...interface{}) error {
	return l.logInternal(DEBUG, msg, keyvals...)
}

// Info writes a best-effort INFO record and publishes failures through Health.
func (l *Logger) Info(msg string, keyvals ...interface{}) {
	_ = l.InfoChecked(msg, keyvals...)
}

// InfoChecked writes an INFO record and returns any configured-sink failure.
func (l *Logger) InfoChecked(msg string, keyvals ...interface{}) error {
	return l.logInternal(INFO, msg, keyvals...)
}

// Warn writes a best-effort WARN record and publishes failures through Health.
func (l *Logger) Warn(msg string, keyvals ...interface{}) {
	_ = l.WarnChecked(msg, keyvals...)
}

// WarnChecked writes a WARN record and returns any configured-sink failure.
func (l *Logger) WarnChecked(msg string, keyvals ...interface{}) error {
	return l.logInternal(WARN, msg, keyvals...)
}

// Error writes a best-effort ERROR record and publishes failures through Health.
func (l *Logger) Error(msg string, keyvals ...interface{}) {
	_ = l.ErrorChecked(msg, keyvals...)
}

// ErrorChecked writes an ERROR record and returns any configured-sink failure.
func (l *Logger) ErrorChecked(msg string, keyvals ...interface{}) error {
	return l.logInternal(ERROR, msg, keyvals...)
}

// WithField returns a logger view with one additional immutable field.
func (l *Logger) WithField(key string, value interface{}) *Logger {
	newLogger := &Logger{
		state:  l.state,
		fields: make(map[string]interface{}, len(l.fields)+1),
	}

	// Copy existing fields so sibling views cannot mutate each other.
	for k, v := range l.fields {
		newLogger.fields[k] = v
	}
	newLogger.fields[key] = value

	return newLogger
}

// SetLevel changes the runtime log level without waiting for sink I/O.
func (l *Logger) SetLevel(level string) {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	l.state.level = parseLogLevel(level)
}

// Close closes the active log sink after preceding writes finish.
func (l *Logger) Close() error {
	leaveWrite := l.state.writeGate.Enter()
	defer leaveWrite()

	if l.state.useSyslog && l.state.syslogWriter != nil {
		if err := l.state.syslogWriter.Close(); err != nil {
			return l.reportSinkFailure("close", err)
		}
		return nil
	}

	if l.state.file != nil {
		if err := l.state.operations().close(l.state.file); err != nil {
			return l.reportSinkFailure("close", err)
		}
		l.state.file = nil
	}
	return nil
}
