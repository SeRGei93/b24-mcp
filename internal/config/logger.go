package config

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

var redactedKeys = map[string]struct{}{
	"access_token":       {},
	"refresh_token":      {},
	"accessToken":        {},
	"refreshToken":       {},
	"authorization":      {},
	"client_secret":      {},
	"bitrixClientSecret": {},
	"mcpAuthToken":       {},
	"tokenEncryptionKey": {},
	"code":               {},
	"code_verifier":      {},
	"token":              {},
	"auth":               {},
}

func RedactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return redactMap(v, func(val any) any { return RedactValue(val) })
	case map[string]string:
		return redactMap(v, func(val string) any { return val })
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RedactValue(item)
		}
		return out
	default:
		return value
	}
}

func redactMap[V any](in map[string]V, transform func(V) any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if _, ok := redactedKeys[k]; ok {
			out[k] = "[REDACTED]"
		} else {
			out[k] = transform(v)
		}
	}
	return out
}

var logLevelPriority = map[string]int{
	"trace":  10,
	"debug":  20,
	"info":   30,
	"warn":   40,
	"error":  50,
	"fatal":  60,
	"silent": 100,
}

type Logger struct {
	mu       sync.Mutex
	out      io.Writer
	minLevel int
}

func NewLogger(out io.Writer, level string) *Logger {
	prio, ok := logLevelPriority[level]
	if !ok {
		prio = logLevelPriority["info"]
	}
	return &Logger{out: out, minLevel: prio}
}

func StderrLogger(level string) *Logger {
	return NewLogger(os.Stderr, level)
}

func (l *Logger) Debug(message string, fields map[string]any) {
	l.write("debug", message, fields)
}

func (l *Logger) Info(message string, fields map[string]any) {
	l.write("info", message, fields)
}

func (l *Logger) Warn(message string, fields map[string]any) {
	l.write("warn", message, fields)
}

func (l *Logger) Error(message string, fields map[string]any) {
	l.write("error", message, fields)
}

func (l *Logger) write(level, message string, fields map[string]any) {
	if l == nil || l.out == nil {
		return
	}
	if logLevelPriority[level] < l.minLevel {
		return
	}
	payload := map[string]any{
		"level":   level,
		"time":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"message": message,
	}
	if fields != nil {
		redacted, _ := RedactValue(fields).(map[string]any)
		for k, v := range redacted {
			payload[k] = v
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(encoded, '\n'))
}
