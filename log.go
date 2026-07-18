package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type LogLevel int

const (
	LvlDebug LogLevel = iota
	LvlInfo
	LvlWarn
	LvlError
)

var levelNames = map[string]LogLevel{
	"debug": LvlDebug,
	"info":  LvlInfo,
	"warn":  LvlWarn,
	"error": LvlError,
}

type Logger struct {
	level LogLevel
	mu    sync.Mutex
}

func NewLogger(levelStr string) *Logger {
	lvl, ok := levelNames[levelStr]
	if !ok {
		lvl = LvlInfo
	}
	return &Logger{level: lvl}
}

func (l *Logger) ts() string {
	return time.Now().Format(time.RFC3339)
}

func (l *Logger) log(lvl LogLevel, color, tag string, a ...any) {
	if lvl < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := fmt.Sprintf("\x1b[%sm[%s %s]\x1b[0m", color, tag, l.ts())
	fmt.Fprintln(os.Stdout, append([]any{prefix}, a...)...)
}

func (l *Logger) Debug(a ...any) { l.log(LvlDebug, "90", "DBG", a...) }
func (l *Logger) Info(a ...any)  { l.log(LvlInfo, "36", "INF", a...) }
func (l *Logger) Warn(a ...any)  { l.log(LvlWarn, "33", "WRN", a...) }
func (l *Logger) Error(a ...any) { l.log(LvlError, "31", "ERR", a...) }

// ── 失败限频（同 key 在窗口期内只打一次汇总日志，避免刷屏）──────────
type failRateLimiter struct {
	mu      sync.Mutex
	records map[string]*failRecord
	muteDur time.Duration
}

type failRecord struct {
	count   int
	firstAt time.Time
}

func newFailRateLimiter(muteDur time.Duration) *failRateLimiter {
	f := &failRateLimiter{
		records: make(map[string]*failRecord),
		muteDur: muteDur,
	}
	go f.gcLoop()
	return f
}

func (f *failRateLimiter) gcLoop() {
	ticker := time.NewTicker(f.muteDur)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		f.mu.Lock()
		for k, rec := range f.records {
			if now.Sub(rec.firstAt) > f.muteDur*2 {
				delete(f.records, k)
			}
		}
		f.mu.Unlock()
	}
}

// shouldLog 返回是否应该打印日志，以及汇总计数（0 表示首次触发，直接打印即可）
func (f *failRateLimiter) shouldLog(key string) (log bool, count int) {
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key]
	if !ok {
		f.records[key] = &failRecord{count: 1, firstAt: now}
		return true, 0
	}
	rec.count++
	if now.Sub(rec.firstAt) >= f.muteDur {
		c := rec.count
		f.records[key] = &failRecord{count: 0, firstAt: now}
		return true, c
	}
	return false, 0
}
