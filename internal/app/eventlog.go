package app

import (
	"sync"
	"time"
)

// Event 是面板和日志共用的一条运行事件。
type Event struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Level      string    `json:"level"` // info | warn | error
	Kind       string    `json:"kind"`  // request | failover | health | config | probe | start
	Group      string    `json:"group,omitempty"`
	Upstream   string    `json:"upstream,omitempty"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Status     int       `json:"status,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Message    string    `json:"message"`
}

// EventLog 是固定容量的环形事件缓冲，供面板增量拉取。
type EventLog struct {
	mu   sync.RWMutex
	buf  []Event
	max  int
	next uint64
}

func NewEventLog(capacity int) *EventLog {
	if capacity <= 0 {
		capacity = 500
	}
	return &EventLog{buf: make([]Event, 0, capacity), max: capacity}
}

func (l *EventLog) Add(e Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	e.Seq = l.next
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if len(l.buf) == l.max {
		copy(l.buf, l.buf[1:])
		l.buf[len(l.buf)-1] = e
	} else {
		l.buf = append(l.buf, e)
	}
	return e
}

// Recent 返回最近 n 条（按时间正序）。
func (l *EventLog) Recent(n int) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > len(l.buf) {
		n = len(l.buf)
	}
	out := make([]Event, n)
	copy(out, l.buf[len(l.buf)-n:])
	return out
}

// Since 返回 seq 之后的事件，最多 limit 条。
func (l *EventLog) Since(seq uint64, limit int) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if limit <= 0 {
		limit = 200
	}
	out := make([]Event, 0, limit)
	for i := len(l.buf) - 1; i >= 0; i-- {
		if len(out) >= limit {
			break
		}
		if l.buf[i].Seq <= seq {
			break
		}
		out = append(out, l.buf[i])
	}
	// 反转为正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// LastSeq 返回当前最大序号。
func (l *EventLog) LastSeq() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.next
}
