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
	"fmt"
	"log"
	"log/syslog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// LogLevel rappresenta i livelli di log supportati.
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
)

var (
	// levelNames mappa i livelli di log alle loro stringhe.
	levelNames = map[LogLevel]string{
		DEBUG: "DEBUG",
		INFO:  "INFO",
		WARN:  "WARN",
		ERROR: "ERROR",
	}

	// currentLogger è il logger globale singleton.
	currentLogger *Logger
	once          sync.Once
)

// Logger è il nostro logger personalizzato con rotazione.
type Logger struct {
	state  *loggerState
	fields map[string]interface{} // Campi contestuali per WithField
}

type loggerState struct {
	mu           sync.Mutex
	level        LogLevel
	file         *os.File
	filePath     string
	maxSize      int64
	logger       *log.Logger
	lastRotation time.Time
	useSyslog    bool
	syslogWriter *syslog.Writer
}

// InitLogger inizializza il logger globale con i parametri specificati.
// Deve essere chiamato all'avvio dell'applicazione.
func InitLogger(level string, filePath string, maxSize int, useSyslog bool) {
	once.Do(func() {
		logLevel := parseLogLevel(level)

		// Se syslog è abilitato, crea logger syslog
		if useSyslog {
			syslogWriter, err := syslog.New(syslog.LOG_DAEMON|syslog.LOG_INFO, "resman")
			if err != nil {
				log.Printf("ERROR: Failed to initialize syslog: %v", err)
				// Fallback to stderr so protocol streams on stdout remain clean.
				currentLogger = createStderrLogger(logLevel)
				return
			}

			// Crea logger con syslog
			currentLogger = &Logger{
				state: &loggerState{
					level:        logLevel,
					logger:       log.New(syslogWriter, "", 0),
					useSyslog:    true,
					syslogWriter: syslogWriter,
				},
				fields: make(map[string]interface{}),
			}

			// Logga il primo messaggio via syslog
			currentLogger.logInternal(INFO, "Logger initialized (syslog)",
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

		// Logga il primo messaggio
		currentLogger.logInternal(INFO, "Logger initialized",
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

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", filePath, err)
	}

	return &Logger{
		state: &loggerState{
			level:        level,
			file:         file,
			filePath:     filePath,
			maxSize:      maxSize,
			logger:       log.New(file, "", 0),
			lastRotation: time.Now(),
		},
		fields: make(map[string]interface{}),
	}, nil
}

// GetLogger restituisce il logger globale inizializzato.
func GetLogger() *Logger {
	if currentLogger == nil {
		// If uninitialized, create the default logger with stderr fallback.
		InitLogger("INFO", "/var/log/resman.log", 10*1024*1024, false)
	}
	return currentLogger
}

// parseLogLevel converte una stringa in LogLevel.
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
			level:  level,
			logger: log.New(os.Stderr, "", 0),
		},
		fields: make(map[string]interface{}),
	}
}

// logInternal è il metodo interno di logging che gestisce la formattazione e la scrittura.
func (l *Logger) logInternal(level LogLevel, msg string, keyvals ...interface{}) {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()

	if level < l.state.level {
		return
	}

	logMsg := l.formatMessageLocked(level, msg, keyvals...)

	// Se usiamo syslog, gestiamo i livelli appropriati
	if l.state.useSyslog && l.state.syslogWriter != nil {
		// Best-effort syslog write; errors are ignored to avoid logger recursion.
		switch level {
		case DEBUG:
			_ = l.state.syslogWriter.Debug(logMsg)
		case INFO:
			_ = l.state.syslogWriter.Info(logMsg)
		case WARN:
			_ = l.state.syslogWriter.Warning(logMsg)
		case ERROR:
			_ = l.state.syslogWriter.Err(logMsg)
		default:
			_ = l.state.syslogWriter.Info(logMsg)
		}
	} else {
		// Write to the underlying file or stderr logger.
		l.state.logger.Println(logMsg)

		// Verifica e gestisci la rotazione del log (solo per file-based logger)
		if l.state.file != nil {
			l.checkAndRotateLocked()
		}
	}
}

func (l *Logger) formatMessageLocked(level LogLevel, msg string, keyvals ...interface{}) string {
	// Formatta il messaggio con timestamp e livello
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMsg := fmt.Sprintf("[%s] [%s] %s", timestamp, levelNames[level], sanitizeLogValue(msg))

	// Aggiungi i campi contestuali di WithField
	for k, v := range l.fields {
		logMsg += fmt.Sprintf(" %s=%s", sanitizeLogValue(k), sanitizeLogValue(v))
	}

	// Aggiungi coppie chiave-valore se presenti
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

// checkAndRotate verifica se è necessaria la rotazione e la esegue.
func (l *Logger) checkAndRotateLocked() {
	// Verifica solo una volta al secondo per performance
	if time.Since(l.state.lastRotation) < time.Second {
		return
	}

	l.state.lastRotation = time.Now()

	// Ottieni le dimensioni del file
	info, err := l.state.file.Stat()
	if err != nil {
		// Non possiamo verificare, uscire
		return
	}

	// Se il file supera la dimensione massima, ruota
	if info.Size() > l.state.maxSize {
		l.rotateLogLocked()
	}
}

// rotateLog esegue la rotazione del file di log.
func (l *Logger) rotateLogLocked() {
	// Chiudi il file corrente
	_ = l.state.file.Close()

	// Rinomina il file corrente (es. .log -> .log.1)
	backupPath := l.state.filePath + ".1"

	// Rimuovi il backup precedente se esiste
	if _, err := os.Stat(backupPath); err == nil {
		_ = os.Remove(backupPath)
	}

	// Rinomina il file corrente
	if err := os.Rename(l.state.filePath, backupPath); err != nil {
		// The standard logger reports rotation errors on stderr.
		log.Printf("ERROR: Failed to rotate log file: %v", err)
	}

	// Riapri il nuovo file di log
	file, err := os.OpenFile(l.state.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// If the file cannot be reopened, use stderr to keep stdout clean.
		log.Printf("ERROR: Failed to reopen log file after rotation: %v", err)
		l.state.file = nil
		l.state.logger = log.New(os.Stderr, "", 0)
		return
	}

	l.state.file = file
	l.state.logger.SetOutput(file)

	if INFO >= l.state.level {
		l.state.logger.Println(l.formatMessageLocked(INFO, "Log rotated due to size limit"))
	}
}

// Metodi pubblici per i diversi livelli di log

// Debug logga un messaggio a livello DEBUG.
func (l *Logger) Debug(msg string, keyvals ...interface{}) {
	l.logInternal(DEBUG, msg, keyvals...)
}

// Info logga un messaggio a livello INFO.
func (l *Logger) Info(msg string, keyvals ...interface{}) {
	l.logInternal(INFO, msg, keyvals...)
}

// Warn logga un messaggio a livello WARN.
func (l *Logger) Warn(msg string, keyvals ...interface{}) {
	l.logInternal(WARN, msg, keyvals...)
}

// Error logga un messaggio a livello ERROR.
func (l *Logger) Error(msg string, keyvals ...interface{}) {
	l.logInternal(ERROR, msg, keyvals...)
}

// WithField crea un nuovo logger con un campo aggiuntivo.
func (l *Logger) WithField(key string, value interface{}) *Logger {
	newLogger := &Logger{
		state:  l.state,
		fields: make(map[string]interface{}, len(l.fields)+1),
	}

	// Copia i campi esistenti
	for k, v := range l.fields {
		newLogger.fields[k] = v
	}
	// Aggiungi il nuovo campo
	newLogger.fields[key] = value

	return newLogger
}

// SetLevel cambia il livello di log a runtime.
func (l *Logger) SetLevel(level string) {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	l.state.level = parseLogLevel(level)
}

// Close chiude il file di log se aperto.
func (l *Logger) Close() error {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()

	if l.state.useSyslog && l.state.syslogWriter != nil {
		return l.state.syslogWriter.Close()
	}

	if l.state.file != nil {
		return l.state.file.Close()
	}
	return nil
}
