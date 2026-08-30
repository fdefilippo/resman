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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const maxFailureTextRunes = 512

// Failure describes the last primary logging sink failure without retaining
// the log record that triggered it.
type Failure struct {
	Timestamp     time.Time
	Sink          string
	Operation     string
	Error         string
	FallbackError string
}

// Health is the bounded, queryable health state of the configured log sink.
type Health struct {
	Healthy             bool
	TotalFailures       uint64
	ConsecutiveFailures uint64
	LastFailure         Failure
}

// Health returns a detached snapshot of the configured sink health.
func (l *Logger) Health() Health {
	l.state.healthMu.RLock()
	health := l.state.health
	l.state.healthMu.RUnlock()
	if health.TotalFailures == 0 {
		health.Healthy = true
	}
	return health
}

func (l *Logger) recordSinkSuccess() {
	l.state.healthMu.Lock()
	l.state.health.Healthy = true
	l.state.health.ConsecutiveFailures = 0
	l.state.healthMu.Unlock()
}

func (l *Logger) recordSinkFailure(failure Failure) {
	l.state.healthMu.Lock()
	l.state.health.Healthy = false
	l.state.health.TotalFailures++
	l.state.health.ConsecutiveFailures++
	l.state.health.LastFailure = failure
	l.state.healthMu.Unlock()
}

func (l *Logger) recordFallbackFailure(err error) {
	l.state.healthMu.Lock()
	l.state.health.LastFailure.FallbackError = boundedFailureText(err)
	l.state.healthMu.Unlock()
}

func boundedFailureText(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	runes := []rune(text)
	if len(runes) <= maxFailureTextRunes {
		return text
	}
	return string(runes[:maxFailureTextRunes-3]) + "..."
}

func (l *Logger) sinkName() string {
	switch {
	case l.state.useSyslog:
		return "syslog"
	case l.state.filePath != "":
		return "file"
	default:
		return "stderr"
	}
}

func (l *Logger) fallbackWriter() io.Writer {
	if l.state.fallback != nil {
		return l.state.fallback
	}
	return defaultFallbackWriter
}

func (l *Logger) reportSinkFailure(operation string, primaryErr error) error {
	failure := Failure{
		Timestamp: time.Now().UTC(),
		Sink:      l.sinkName(),
		Operation: operation,
		Error:     boundedFailureText(primaryErr),
	}
	l.recordSinkFailure(failure)

	writer := l.fallbackWriter()
	_, fallbackErr := fmt.Fprintf(
		writer,
		"[%s] [ERROR] Logging sink failure sink=%q operation=%q error=%q\n",
		failure.Timestamp.Format(time.RFC3339Nano),
		failure.Sink,
		failure.Operation,
		failure.Error,
	)
	if fallbackErr != nil {
		l.recordFallbackFailure(fallbackErr)
	}

	primary := fmt.Errorf("logging %s %s failed: %w", failure.Sink, operation, primaryErr)
	if fallbackErr == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("logging fallback failed: %w", fallbackErr))
}
