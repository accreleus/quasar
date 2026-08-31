package telemetry

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Fake is an in-memory Store for tests that exercise handlers, ingest paths or
// the retention policy without a database.
//
// It is a real implementation, not a stub: the ordering, the clamp, the
// source-scoped Latest, the nil-clock-means-unmeasured rule and the retention
// decision all behave as the Postgres adapter does — which is the point, because
// the pair is what proves Policy.Decide is the single description of retention.
// What it deliberately does NOT model is the database: no FK cascade, no uuid
// validation, no concurrency beyond its own mutex.
type Fake struct {
	mu sync.Mutex

	samples map[string][]fakeSample
	events  map[string][]fakeEvent
	clocks  map[string]*Clock

	// sessions is the session state the retention policy needs. A session id
	// absent from this map is treated as NON-TERMINAL, so a test that only cares
	// about ingest never has to declare one.
	sessions map[string]fakeSession

	// Now is the clock. Tests set it to make retention deterministic; the zero
	// value means time.Now.
	Now func() time.Time
}

type fakeSample struct {
	Sample
	createdAt time.Time
}

type fakeEvent struct {
	Event
	createdAt time.Time
	id        string
}

type fakeSession struct {
	terminal bool
	endedAt  time.Time
}

// NewFake builds an empty in-memory store.
func NewFake() *Fake {
	return &Fake{
		samples:  map[string][]fakeSample{},
		events:   map[string][]fakeEvent{},
		clocks:   map[string]*Clock{},
		sessions: map[string]fakeSession{},
	}
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// --- test-only shaping --------------------------------------------------------

// SetSessionState declares whether a session is terminal, and when it ended.
// Only the retention policy reads it.
func (f *Fake) SetSessionState(sessionID string, terminal bool, endedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[sessionID] = fakeSession{terminal: terminal, endedAt: endedAt}
}

// Backdate rewrites the ingestion clock of everything already stored for a
// session, so a test can age rows without sleeping.
func (f *Fake) Backdate(sessionID string, createdAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.samples[sessionID] {
		f.samples[sessionID][i].createdAt = createdAt
	}
	for i := range f.events[sessionID] {
		f.events[sessionID][i].createdAt = createdAt
	}
}

// CountSamples / CountEvents / HasClock are the assertions a retention test needs.
func (f *Fake) CountSamples(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.samples[sessionID])
}

func (f *Fake) CountEvents(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events[sessionID])
}

func (f *Fake) HasClock(sessionID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clocks[sessionID] != nil
}

// --- appends ------------------------------------------------------------------

func (f *Fake) Append(ctx context.Context, sessionID, source string, in SampleInput) error {
	return f.AppendBatch(ctx, sessionID, source, []SampleInput{in})
}

func (f *Fake) AppendBatch(_ context.Context, sessionID, source string, samples []SampleInput) error {
	if len(samples) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for _, s := range samples {
		f.samples[sessionID] = append(f.samples[sessionID], fakeSample{
			Sample:    Sample{Source: source, TsUnixMs: s.TsUnixMs, Metrics: defaultJSON(s.Metrics)},
			createdAt: now,
		})
	}
	return nil
}

func (f *Fake) AppendEvent(ctx context.Context, sessionID, source string, e EventInput) error {
	_, err := f.AppendEventReturningID(ctx, sessionID, source, e)
	return err
}

func (f *Fake) AppendEventReturningID(_ context.Context, sessionID, source string, e EventInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "evt-" + strconv.Itoa(len(f.events[sessionID])+1)
	f.events[sessionID] = append(f.events[sessionID], fakeEvent{
		Event:     Event{Source: source, TsUnixMs: e.TsUnixMs, Type: e.Type, Payload: defaultJSON(e.Payload)},
		createdAt: f.now(),
		id:        id,
	})
	return id, nil
}

func (f *Fake) AppendEvents(ctx context.Context, sessionID, source string, events []EventInput) error {
	for _, e := range events {
		if _, err := f.AppendEventReturningID(ctx, sessionID, source, e); err != nil {
			return err
		}
	}
	return nil
}

// --- clock ---------------------------------------------------------------------

func (f *Fake) UpsertClock(_ context.Context, sessionID string, clientOffsetMs, uncertaintyMs float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.clocks[sessionID] = &Clock{
		ClientOffsetMs: clientOffsetMs,
		UncertaintyMs:  uncertaintyMs,
		MeasuredAt:     now,
		UpdatedAt:      now,
	}
	return nil
}

func (f *Fake) Clock(_ context.Context, sessionID string) (*Clock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clocks[sessionID]
	if c == nil {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

// --- reads ----------------------------------------------------------------------

func inRange(ts int64, r Range) bool {
	if r.FromMs > 0 && ts < r.FromMs {
		return false
	}
	if r.ToMs > 0 && ts > r.ToMs {
		return false
	}
	return true
}

func (f *Fake) Window(ctx context.Context, sessionID string, r Range, fl Filter) (Slice, error) {
	samples, err := f.samplesInWindow(sessionID, r, fl.Limit)
	if err != nil {
		return Slice{}, err
	}
	events, err := f.Events(ctx, sessionID, r, fl)
	if err != nil {
		return Slice{}, err
	}
	clock, err := f.Clock(ctx, sessionID)
	if err != nil {
		return Slice{}, err
	}
	return Slice{Samples: samples, Events: events, Clock: clock}, nil
}

func (f *Fake) samplesInWindow(sessionID string, r Range, limit int32) ([]Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Sample
	for _, s := range f.samples[sessionID] {
		if inRange(s.TsUnixMs, r) {
			out = append(out, s.Sample)
		}
	}
	sortSamplesNewestFirst(out)
	return truncate(out, clampLimit(limit)), nil
}

func (f *Fake) Events(_ context.Context, sessionID string, r Range, fl Filter) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]bool{}
	for _, t := range fl.Types {
		want[t] = true
	}
	var out []Event
	for _, e := range f.events[sessionID] {
		if !inRange(e.TsUnixMs, r) {
			continue
		}
		if len(want) > 0 && !want[e.Type] {
			continue
		}
		out = append(out, e.Event)
	}
	sortEventsNewestFirst(out)
	return truncate(out, clampLimit(fl.Limit)), nil
}

func (f *Fake) Recent(_ context.Context, sessionID string, limit int32, source *string, cursor string) ([]Sample, string, error) {
	limit = clampLimit(limit)
	var offset int64
	if cursor != "" {
		offset, _ = strconv.ParseInt(cursor, 10, 64)
	}
	f.mu.Lock()
	var all []Sample
	for _, s := range f.samples[sessionID] {
		if source != nil && s.Source != *source {
			continue
		}
		all = append(all, s.Sample)
	}
	f.mu.Unlock()
	sortSamplesNewestFirst(all)
	if offset >= int64(len(all)) {
		return nil, "", nil
	}
	page := all[offset:]
	var next string
	if int64(len(page)) > int64(limit) {
		page = page[:limit]
		next = strconv.FormatInt(offset+int64(limit), 10)
	}
	return page, next, nil
}

func (f *Fake) LatestPerSession(_ context.Context, sessionIDs []string) (map[string]Latest, error) {
	out := map[string]Latest{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sid := range sessionIDs {
		var l Latest
		for _, s := range f.samples[sid] {
			sample := s.Sample
			switch sample.Source {
			case SourceAgent:
				if l.Agent == nil || sample.TsUnixMs > l.Agent.TsUnixMs {
					cp := sample
					l.Agent = &cp
				}
			case SourceBrowser:
				if l.Browser == nil || sample.TsUnixMs > l.Browser.TsUnixMs {
					cp := sample
					l.Browser = &cp
				}
			}
		}
		if l.Agent != nil || l.Browser != nil {
			out[sid] = l
		}
	}
	return out, nil
}

func (f *Fake) Captures(_ context.Context, sessionID string) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Event
	for _, e := range f.events[sessionID] {
		if IsCapture(e.Type) {
			out = append(out, e.Event)
		}
	}
	sortEventsNewestFirst(out)
	return truncate(out, maxCapturesPerSession), nil
}

func (f *Fake) Capture(_ context.Context, sessionID, captureID string) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *Event
	for i := range f.events[sessionID] {
		e := f.events[sessionID][i]
		if !IsCapture(e.Type) {
			continue
		}
		var p struct {
			CaptureID string `json:"capture_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.CaptureID != captureID {
			continue
		}
		if found == nil || e.TsUnixMs > found.TsUnixMs {
			cp := e.Event
			found = &cp
		}
	}
	if found == nil {
		return Event{}, ErrNotFound
	}
	return *found, nil
}

// --- retention -------------------------------------------------------------------

// Retain applies exactly Policy.Decide, row by row. Where the Postgres adapter
// expresses the policy as five statements, this expresses it as the function
// those statements are meant to implement.
func (f *Fake) Retain(_ context.Context, p Policy) (Report, error) {
	p = p.normalized()
	f.mu.Lock()
	defer f.mu.Unlock()
	started := f.now()
	var rep Report

	for sid, samples := range f.samples {
		sess := f.sessions[sid]
		kept := samples[:0]
		for _, s := range samples {
			switch p.Decide(Row{
				CreatedAt:       s.createdAt,
				SessionTerminal: sess.terminal,
				SessionEndedAt:  sess.endedAt,
			}, started) {
			case SweepRolling:
				rep.RollingSamples++
			case SweepPostMortem:
				rep.PostMortemSamples++
			default:
				kept = append(kept, s)
			}
		}
		f.samples[sid] = kept
	}

	for sid, events := range f.events {
		sess := f.sessions[sid]
		kept := events[:0]
		for _, e := range events {
			switch p.Decide(Row{
				CreatedAt:       e.createdAt,
				SessionTerminal: sess.terminal,
				SessionEndedAt:  sess.endedAt,
				Type:            e.Type,
			}, started) {
			case SweepRolling:
				rep.RollingEvents++
			case SweepPostMortem:
				rep.PostMortemEvents++
			default:
				kept = append(kept, e)
			}
		}
		f.events[sid] = kept
	}

	for sid, c := range f.clocks {
		if c == nil {
			continue
		}
		sess := f.sessions[sid]
		// The clock row has no ingestion clock of its own worth reasoning about —
		// it is one row per session, refreshed in place — so it rides the session's
		// post-mortem expiry only, exactly as the SQL does.
		if sess.terminal && started.Sub(sess.endedAt) >= p.PostMortem {
			delete(f.clocks, sid)
			rep.PostMortemClocks++
		}
	}

	rep.Duration = f.now().Sub(started)
	return rep, nil
}

// --- ordering helpers --------------------------------------------------------------

func sortSamplesNewestFirst(in []Sample) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].TsUnixMs > in[j].TsUnixMs })
}

func sortEventsNewestFirst(in []Event) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].TsUnixMs > in[j].TsUnixMs })
}

func truncate[T any](in []T, limit int32) []T {
	if int32(len(in)) > limit {
		return in[:limit]
	}
	return in
}
