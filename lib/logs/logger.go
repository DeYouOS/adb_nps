//go:build android
// +build android

package logs

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	Logger       = log.New(io.Discard, "", 0)
	ZapLogger    any
	bufferWriter *BufferWriter
	logWriter    io.Writer = io.Discard
	minLevel               = levelInfo
)

const (
	defaultBufSize = 64 * 1024
	levelTrace     = 7
	levelDebug     = 6
	levelInfo      = 5
	levelWarn      = 4
	levelError     = 3
	levelFatal     = 2
	levelPanic     = 1
	levelOff       = 0
)

type BufferWriter struct {
	mu    sync.Mutex
	buf   []byte
	cap   int
	start int
	size  int
}

func NewBufferWriter(capacity int) *BufferWriter {
	if capacity <= 0 {
		capacity = defaultBufSize
	}
	return &BufferWriter{buf: make([]byte, capacity), cap: capacity}
}

func (w *BufferWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) >= w.cap {
		copy(w.buf, p[len(p)-w.cap:])
		w.start = 0
		w.size = w.cap
		return len(p), nil
	}
	if w.size+len(p) > w.cap {
		drop := w.size + len(p) - w.cap
		w.start = (w.start + drop) % w.cap
		w.size -= drop
	}
	writePos := (w.start + w.size) % w.cap
	written := copy(w.buf[writePos:], p)
	if written < len(p) {
		copy(w.buf, p[written:])
	}
	w.size += len(p)
	return len(p), nil
}

func (w *BufferWriter) GetAndClear() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size == 0 {
		return ""
	}
	var s string
	if w.start+w.size <= w.cap {
		s = string(w.buf[w.start : w.start+w.size])
	} else {
		tmp := make([]byte, w.size)
		n := copy(tmp, w.buf[w.start:])
		copy(tmp[n:], w.buf[:w.size-n])
		s = string(tmp)
	}
	w.start = 0
	w.size = 0
	return s
}

func EnableInMemoryBuffer(capacity int) {
	if bufferWriter == nil {
		bufferWriter = NewBufferWriter(capacity)
	}
}

func GetBufferedLogs() string {
	if bufferWriter == nil {
		return ""
	}
	return bufferWriter.GetAndClear()
}

func Init(logType, logLevel, logPath string, maxSize, maxBackups, maxAge int, compress bool, color bool) {
	_, _, _, _, _ = maxSize, maxBackups, maxAge, compress, color
	minLevel = parseLevel(logLevel)
	if strings.EqualFold(logType, "off") || minLevel == levelOff {
		logWriter = io.Discard
		Logger = log.New(io.Discard, "", 0)
		return
	}

	var writers []io.Writer
	if strings.EqualFold(logType, "stdout") || strings.EqualFold(logType, "both") {
		writers = append(writers, os.Stdout)
	}
	if (strings.EqualFold(logType, "file") || strings.EqualFold(logType, "both")) && logPath != "" && logPath != "/dev/null" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				writers = append(writers, f)
			}
		}
	}
	if bufferWriter != nil {
		writers = append(writers, bufferWriter)
	}
	if len(writers) == 0 {
		logWriter = io.Discard
	} else if len(writers) == 1 {
		logWriter = writers[0]
	} else {
		logWriter = io.MultiWriter(writers...)
	}
	Logger = log.New(logWriter, "", 0)
}

func parseLevel(level string) int {
	switch strings.ToLower(level) {
	case "trace":
		return levelTrace
	case "debug":
		return levelDebug
	case "info", "informational", "notice":
		return levelInfo
	case "warn", "warning":
		return levelWarn
	case "error":
		return levelError
	case "fatal":
		return levelFatal
	case "panic":
		return levelPanic
	case "off", "disabled", "0":
		return levelOff
	default:
		return levelInfo
	}
}

func output(level int, prefix, msg string, v ...interface{}) {
	if minLevel < level || logWriter == io.Discard {
		return
	}
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format(time.RFC3339), prefix, fmt.Sprintf(msg, v...))
	_, _ = io.WriteString(logWriter, line)
	if bufferWriter != nil {
		_, _ = bufferWriter.Write([]byte(line))
	}
}

func Trace(msg string, v ...interface{})  { output(levelTrace, "TRACE", msg, v...) }
func Debug(msg string, v ...interface{})  { output(levelDebug, "DEBUG", msg, v...) }
func Info(msg string, v ...interface{})   { output(levelInfo, "INFO", msg, v...) }
func Warn(msg string, v ...interface{})   { output(levelWarn, "WARN", msg, v...) }
func Error(msg string, v ...interface{})  { output(levelError, "ERROR", msg, v...) }
func Fatal(msg string, v ...interface{})  { output(levelFatal, "FATAL", msg, v...); os.Exit(1) }
func Panic(msg string, v ...interface{})  { panic(fmt.Sprintf(msg, v...)) }
func Println(v ...interface{})            { output(levelInfo, "INFO", "%s", fmt.Sprintln(v...)) }
func Print(v ...interface{})              { output(levelInfo, "INFO", "%s", fmt.Sprint(v...)) }
func Printf(msg string, v ...interface{}) { output(levelInfo, "INFO", msg, v...) }
func SetLevel(levelStr string)            { minLevel = parseLevel(levelStr) }
