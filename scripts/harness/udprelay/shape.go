package main

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// Bursts is a schedule of full-drop windows: Count windows of DropMs, one every EveryMs, the first at apply time.
type Bursts struct {
	DropMs  float64 `json:"drop_ms,omitempty"`
	Count   int     `json:"count,omitempty"`
	EveryMs float64 `json:"every_ms,omitempty"`
}

// Shape is one direction's impairment. Zero fields are off. Order of the stages: blackout, loss, policer,
// rate cap with queue, delay. A new Shape replaces the old one whole.
type Shape struct {
	// tbf semantics: a token bucket of BurstBytes refilled at RateBps, packets wait in a FIFO and are dropped
	// when it is full. The queue limit is LimitBytes, or tc's rate*LatencyMs + burst.
	RateBps    int64   `json:"rate_bps,omitempty"`
	BurstBytes int64   `json:"burst_bytes,omitempty"`
	LimitBytes int64   `json:"limit_bytes,omitempty"`
	LatencyMs  float64 `json:"latency_ms,omitempty"`
	// policer semantics: a token bucket with no queue, a packet without tokens is dropped.
	PoliceBps        int64 `json:"police_bps,omitempty"`
	PoliceBurstBytes int64 `json:"police_burst_bytes,omitempty"`
	// Delay plus uniform jitter, never reordering unless Reorder is set.
	DelayMs  float64 `json:"delay_ms,omitempty"`
	JitterMs float64 `json:"jitter_ms,omitempty"`
	Reorder  bool    `json:"reorder,omitempty"`
	// Random i.i.d. loss, in percent.
	LossPct float64 `json:"loss_pct,omitempty"`
	// Silent drop of everything: one outage starting now, and/or a burst schedule.
	OutageMs float64 `json:"outage_ms,omitempty"`
	Bursts   *Bursts `json:"bursts,omitempty"`
}

const minBurst = 1500 // one full packet; a smaller bucket would never pass anything

func (s Shape) Validate() error {
	if s.RateBps < 0 || s.PoliceBps < 0 || s.DelayMs < 0 || s.JitterMs < 0 || s.OutageMs < 0 || s.LimitBytes < 0 || s.LatencyMs < 0 {
		return errors.New("negative value")
	}
	if s.LossPct < 0 || s.LossPct > 100 {
		return errors.New("loss_pct outside 0..100")
	}
	if s.RateBps > 0 && (s.BurstBytes < minBurst || (s.LimitBytes == 0 && s.LatencyMs == 0)) {
		return errors.New("rate_bps needs burst_bytes >= 1500 and limit_bytes or latency_ms")
	}
	if s.PoliceBps > 0 && s.PoliceBurstBytes < minBurst {
		return errors.New("police_bps needs police_burst_bytes >= 1500")
	}
	if b := s.Bursts; b != nil && (b.DropMs <= 0 || b.Count <= 0 || b.EveryMs < b.DropMs) {
		return errors.New("bursts needs drop_ms > 0, count > 0, every_ms >= drop_ms")
	}
	return nil
}

func (s Shape) limit() int64 {
	if s.LimitBytes > 0 {
		return s.LimitBytes
	}
	return int64(float64(s.RateBps)/8*s.LatencyMs/1000) + s.BurstBytes
}

// Drop reasons, also the keys of the stats log.
const (
	DropBlackout = "blackout"
	DropLoss     = "loss"
	DropPolice   = "police"
	DropQueue    = "queue"
)

type Count struct {
	Pkts  int64 `json:"pkts"`
	Bytes int64 `json:"bytes"`
}

func (c *Count) add(n int) { c.Pkts++; c.Bytes += int64(n) }

type Counters struct {
	In, Out Count
	Drop    map[string]*Count
}

type window struct{ from, to time.Time }
type queued struct {
	dep time.Time
	n   int
}

// Shaper decides, per packet and at arrival, when the packet leaves (or that it is dropped). It owns no timers
// and no sockets, so tests drive it with synthetic times.
type Shaper struct {
	mu  sync.Mutex
	rng *rand.Rand
	sh  Shape
	win []window

	tokens  float64 // tbf bucket, bytes, valid at tlast
	tlast   time.Time
	q       []queued // packets whose tbf departure is still ahead
	backlog int64
	ptokens float64
	plast   time.Time
	lastRel time.Time // kept across Apply so a delay step does not reorder

	cnt Counters
}

func NewShaper(seed int64) *Shaper {
	return &Shaper{rng: rand.New(rand.NewSource(seed)), cnt: Counters{Drop: map[string]*Count{}}}
}

func (s *Shaper) Apply(now time.Time, sh Shape) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sh = sh
	s.win = s.win[:0]
	if sh.OutageMs > 0 {
		s.win = append(s.win, window{now, now.Add(ms(sh.OutageMs))})
	}
	if b := sh.Bursts; b != nil {
		for i := 0; i < b.Count; i++ {
			from := now.Add(ms(b.EveryMs * float64(i)))
			s.win = append(s.win, window{from, from.Add(ms(b.DropMs))})
		}
	}
	s.tokens, s.tlast, s.q, s.backlog = float64(sh.BurstBytes), now, nil, 0
	s.ptokens, s.plast = float64(sh.PoliceBurstBytes), now
}

func (s *Shaper) Shape() Shape { s.mu.Lock(); defer s.mu.Unlock(); return s.sh }

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func (s *Shaper) drop(reason string, n int) (time.Time, string) {
	c := s.cnt.Drop[reason]
	if c == nil {
		c = &Count{}
		s.cnt.Drop[reason] = c
	}
	c.add(n)
	return time.Time{}, reason
}

// Offer takes a packet of n bytes arriving at now. It returns when to release it, or a drop reason.
func (s *Shaper) Offer(now time.Time, n int) (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cnt.In.add(n)
	sh := s.sh
	for _, w := range s.win {
		if !now.Before(w.from) && now.Before(w.to) {
			return s.drop(DropBlackout, n)
		}
	}
	if sh.LossPct > 0 && s.rng.Float64()*100 < sh.LossPct {
		return s.drop(DropLoss, n)
	}
	if sh.PoliceBps > 0 {
		rate := float64(sh.PoliceBps) / 8
		s.ptokens = min(float64(sh.PoliceBurstBytes), s.ptokens+rate*now.Sub(s.plast).Seconds())
		s.plast = now
		if s.ptokens < float64(n) {
			return s.drop(DropPolice, n)
		}
		s.ptokens -= float64(n)
	}
	dep := now
	if sh.RateBps > 0 {
		rate := float64(sh.RateBps) / 8
		for len(s.q) > 0 && !s.q[0].dep.After(now) {
			s.backlog -= int64(s.q[0].n)
			s.q = s.q[1:]
		}
		if s.backlog+int64(n) > sh.limit() {
			return s.drop(DropQueue, n)
		}
		start := now
		if len(s.q) > 0 {
			start = s.tlast // FIFO: behind the last queued packet
		}
		tok := min(float64(sh.BurstBytes), s.tokens+rate*start.Sub(s.tlast).Seconds())
		if tok >= float64(n) {
			dep, tok = start, tok-float64(n)
		} else {
			dep, tok = start.Add(time.Duration((float64(n)-tok)/rate*float64(time.Second))), 0
		}
		s.tokens, s.tlast = tok, dep
		s.q = append(s.q, queued{dep, n})
		s.backlog += int64(n)
	}
	rel := dep
	if sh.DelayMs > 0 || sh.JitterMs > 0 {
		d := sh.DelayMs + (s.rng.Float64()*2-1)*sh.JitterMs
		rel = dep.Add(ms(max(d, 0)))
	}
	if !sh.Reorder {
		rel = maxTime(rel, s.lastRel)
		s.lastRel = rel
	}
	return rel, ""
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// NoteOut counts a packet that really left the relay.
func (s *Shaper) NoteOut(n int) { s.mu.Lock(); s.cnt.Out.add(n); s.mu.Unlock() }

// Snapshot returns the cumulative counters and the tbf backlog at now.
func (s *Shaper) Snapshot(now time.Time) (c Counters, backlogBytes int64, backlogMs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.q) > 0 && !s.q[0].dep.After(now) {
		s.backlog -= int64(s.q[0].n)
		s.q = s.q[1:]
	}
	c = Counters{In: s.cnt.In, Out: s.cnt.Out, Drop: map[string]*Count{}}
	for k, v := range s.cnt.Drop {
		cp := *v
		c.Drop[k] = &cp
	}
	if s.sh.RateBps > 0 {
		backlogMs = float64(s.backlog) / (float64(s.sh.RateBps) / 8) * 1000
	}
	return c, s.backlog, backlogMs
}
