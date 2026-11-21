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
	Timestamp  string    `json:"timestamp"` // date-bearing local display value; time remains RFC3339
}

// EventLog 是固定容量的环形事件缓冲，供面板增量拉取。
type EventLog struct {
	mu         sync.RWMutex
	buf        []Event // compatibility combined ring
	stateBuf   []Event
	requestBuf []Event
	max        int
	next       uint64
}

func NewEventLog(capacity int) *EventLog {
	if capacity <= 0 {
		capacity = 2000
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
	e.Timestamp = e.Time.Format("2006-01-02 15:04:05 -0700")
	if len(l.buf) == l.max {
		copy(l.buf, l.buf[1:])
		l.buf[len(l.buf)-1] = e
	} else {
		l.buf = append(l.buf, e)
	}
	// Only lifecycle events enter the state ring. Request errors/warnings are
	// still request traffic and must not evict configuration or health history.
	switch e.Kind {
	case "failover", "health", "config", "start":
		l.stateBuf = appendEventRing(l.stateBuf, e, l.max)
	default:
		l.requestBuf = appendEventRing(l.requestBuf, e, min(l.max, 500))
	}
	return e
}

func appendEventRing(buf []Event, e Event, max int) []Event {
	if len(buf) == max {
		copy(buf, buf[1:])
		buf[len(buf)-1] = e
		return buf
	}
	return append(buf, e)
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

// Query filters before limiting. Incremental pages return the oldest matching
// events after the cursor; initial reads retain the most-recent-page semantics.
func (l *EventLog) Query(group string, after uint64, limit int) ([]Event, uint64) {
	return l.QueryStream(group, "", "", after, limit)
}

func (l *EventLog) QueryStream(group, stream, kind string, after uint64, limit int) ([]Event, uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	buf := l.buf
	if stream == "state" {
		buf = l.stateBuf
	} else if stream == "request" {
		buf = l.requestBuf
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > len(buf) {
		limit = len(buf)
	}
	out := make([]Event, 0, limit)
	matches := func(e Event) bool {
		return (group == "" || e.Group == group) && (kind == "" || e.Kind == kind)
	}
	if after > 0 {
		for _, e := range buf {
			if e.Seq <= after || !matches(e) {
				continue
			}
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	} else {
		for i := len(buf) - 1; i >= 0 && len(out) < limit; i-- {
			if matches(buf[i]) {
				out = append(out, buf[i])
			}
		}
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if len(out) > 0 {
		return out, out[len(out)-1].Seq
	}
	// An exhausted filter can advance over unrelated events, but a nonempty
	// page must never acknowledge matching events the caller has not received.
	return out, l.next
}

func (l *EventLog) RingStats() map[string]any {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return map[string]any{"state": map[string]int{"count": len(l.stateBuf), "capacity": l.max}, "request": map[string]int{"count": len(l.requestBuf), "capacity": min(l.max, 500)}}
}

func (l *EventLog) Counts() (retained int, total uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.buf), l.next
}

// Resize retains recent events and the sequence counter during reload.
func (l *EventLog) Resize(capacity int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if capacity <= 0 {
		capacity = 2000
	}
	if len(l.buf) > capacity {
		l.buf = append([]Event(nil), l.buf[len(l.buf)-capacity:]...)
	}
	if len(l.stateBuf) > capacity {
		l.stateBuf = append([]Event(nil), l.stateBuf[len(l.stateBuf)-capacity:]...)
	}
	requestCap := min(capacity, 500)
	if len(l.requestBuf) > requestCap {
		l.requestBuf = append([]Event(nil), l.requestBuf[len(l.requestBuf)-requestCap:]...)
	}
	l.max = capacity
}

// LastSeq 返回当前最大序号。
func (l *EventLog) LastSeq() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.next
}
