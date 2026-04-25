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
	mu           sync.Mutex
	level        LogLevel
	file         *os.File
	filePath     string
	maxSize      int64
	logger       *log.Logger
	lastRotation time.Time
	UseSyslog    bool
	syslogWriter *syslog.Writer
	fields       map[string]interface{} // Campi contestuali per WithField
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
				// Fallback a stdout
				currentLogger = createStdoutLogger(logLevel)
				return
			}

			// Crea logger con syslog
			currentLogger = &Logger{
				level:        logLevel,
				file:         nil,
				filePath:     "",
				maxSize:      0,
				logger:       log.New(syslogWriter, "", 0),
				UseSyslog:    true,
				syslogWriter: syslogWriter,
				fields:       make(map[string]interface{}),
			}

			// Logga il primo messaggio via syslog
			currentLogger.logInternal(INFO, "Logger initialized (syslog)",
				"level", levelNames[logLevel],
				"syslog", true,
			)
			return
		}

		// ALTRIMENTI: usa file di log (comportamento originale)
		// Crea la directory del log se non esiste
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			log.Printf("ERROR: Failed to create log directory: %v", err)
			// Fallback a stdout
			currentLogger = createStdoutLogger(logLevel)
			return
		}

		// Apri o crea il file di log
		file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("ERROR: Failed to open log file %s: %v", filePath, err)
			// Fallback a stdout
			currentLogger = createStdoutLogger(logLevel)
			return
		}

		// Crea il logger
		currentLogger = &Logger{
			level:        logLevel,
			file:         file,
			filePath:     filePath,
			maxSize:      int64(maxSize),
			logger:       log.New(file, "", 0),
			lastRotation: time.Now(),
			UseSyslog:    false,
			syslogWriter: nil,
			fields:       make(map[string]interface{}),
		}

		// Logga il primo messaggio
		currentLogger.logInternal(INFO, "Logger initialized",
			"level", levelNames[logLevel],
			"file", filePath,
			"max_size", fmt.Sprintf("%d bytes", maxSize),
		)
	})
}

// GetLogger restituisce il logger globale inizializzato.
func GetLogger() *Logger {
	if currentLogger == nil {
		// Se non inizializzato, crea un logger di default su stdout
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

// createStdoutLogger crea un logger di fallback su stdout.
func createStdoutLogger(level LogLevel) *Logger {
	return &Logger{
		level:     level,
		file:      nil,
		logger:    log.New(os.Stdout, "", 0),
		UseSyslog: false,
		fields:    make(map[string]interface{}),
	}
}

// shouldLog determina se un messaggio del dato livello dovrebbe essere loggato.
func (l *Logger) shouldLog(level LogLevel) bool {
	return level >= l.level
}

// logInternal è il metodo interno di logging che gestisce la formattazione e la scrittura.
func (l *Logger) logInternal(level LogLevel, msg string, keyvals ...interface{}) {
	if !l.shouldLog(level) {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Formatta il messaggio con timestamp e livello
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMsg := fmt.Sprintf("[%s] [%s] %s", timestamp, levelNames[level], msg)

	// Aggiungi i campi contestuali di WithField
	for k, v := range l.fields {
		logMsg += fmt.Sprintf(" %v=%v", k, v)
	}

	// Aggiungi coppie chiave-valore se presenti
	if len(keyvals) > 0 {
		for i := 0; i < len(keyvals); i += 2 {
			if i+1 < len(keyvals) {
				logMsg += fmt.Sprintf(" %v=%v", keyvals[i], keyvals[i+1])
			} else {
				logMsg += fmt.Sprintf(" %v=", keyvals[i])
			}
		}
	}

	// Se usiamo syslog, gestiamo i livelli appropriati
	if l.UseSyslog && l.syslogWriter != nil {
		switch level {
		case DEBUG:
			l.syslogWriter.Debug(logMsg)
		case INFO:
			l.syslogWriter.Info(logMsg)
		case WARN:
			l.syslogWriter.Warning(logMsg)
		case ERROR:
			l.syslogWriter.Err(logMsg)
		default:
			l.syslogWriter.Info(logMsg)
		}
	} else {
		// Scrivi sul logger sottostante (file/stdout)
		l.logger.Println(logMsg)

		// Verifica e gestisci la rotazione del log (solo per file-based logger)
		if l.file != nil {
			l.checkAndRotate()
		}
	}
}

// checkAndRotate verifica se è necessaria la rotazione e la esegue.
func (l *Logger) checkAndRotate() {
	// Verifica solo una volta al secondo per performance
	if time.Since(l.lastRotation) < time.Second {
		return
	}

	l.lastRotation = time.Now()

	// Ottieni le dimensioni del file
	info, err := l.file.Stat()
	if err != nil {
		// Non possiamo verificare, uscire
		return
	}

	// Se il file supera la dimensione massima, ruota
	if info.Size() > l.maxSize {
		l.rotateLog()
	}
}

// rotateLog esegue la rotazione del file di log.
func (l *Logger) rotateLog() {
	// Chiudi il file corrente
	l.file.Close()

	// Rinomina il file corrente (es. .log -> .log.1)
	backupPath := l.filePath + ".1"

	// Rimuovi il backup precedente se esiste
	if _, err := os.Stat(backupPath); err == nil {
		os.Remove(backupPath)
	}

	// Rinomina il file corrente
	if err := os.Rename(l.filePath, backupPath); err != nil {
		// Se la rinomina fallisce, logga l'errore su stdout
		log.Printf("ERROR: Failed to rotate log file: %v", err)
	}

	// Riapri il nuovo file di log
	file, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// Se non possiamo riaprire il file, usiamo stdout
		log.Printf("ERROR: Failed to reopen log file after rotation: %v", err)
		l.file = nil
		l.logger = log.New(os.Stdout, "", 0)
		return
	}

	l.file = file
	l.logger.SetOutput(file)

	// Logga l'evento di rotazione
	l.logInternal(INFO, "Log rotated due to size limit")
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
	l.mu.Lock()
	defer l.mu.Unlock()

	newLogger := &Logger{
		level:        l.level,
		file:         l.file,
		filePath:     l.filePath,
		maxSize:      l.maxSize,
		logger:       l.logger,
		lastRotation: l.lastRotation,
		UseSyslog:    l.UseSyslog,
		syslogWriter: l.syslogWriter,
		fields:       make(map[string]interface{}),
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
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = parseLogLevel(level)
}

// Close chiude il file di log se aperto.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.UseSyslog && l.syslogWriter != nil {
		return l.syslogWriter.Close()
	}

	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
