// Package wroom implements a virtual waiting room: an admission layer that
// lets a safe number of users per second through to the backend and holds
// the rest in a queue, telling them their position.
//
// Two queue policies are supported:
//   - fifo:   users are admitted in arrival order.
//   - random: everyone who arrived before release time is shuffled at release
//     (a pre-queue lottery, as offered by commercial waiting rooms); later
//     arrivals are admitted in arrival order after them.
//
// Two ticket implementations are supported, which differ in what happens when
// a user refreshes the page:
//   - naive: the ticket lives only in the page (returned in the body and sent
//     back by the page's polling script). A manual refresh loses it and the
//     user rejoins at the back.
//   - token: the ticket is also stored in a signed cookie, so a refresh keeps
//     the user's place.
//
// Three optional mechanisms address the failures measured in the first paper
// (admitted users who refresh at an overloaded backend):
//   - a request lease: one admission is good for at most LeaseRequests
//     requests to the backend; after that the user rejoins the queue (at the
//     front by default, so arrival order is kept).
//   - closed-loop admission: the admission rate is adjusted every
//     ControlInterval from the backend's response times (additive increase,
//     multiplicative decrease), instead of being fixed in advance.
//   - a wait estimate: queued responses carry eta_ms, the position divided by
//     the current admission rate.
package wroom

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k144u/nocrashing/resultbench/internal/metrics"
)

// Policy names.
const (
	PolicyFIFO   = "fifo"
	PolicyRandom = "random"
	ModeNaive    = "naive"
	ModeToken    = "token"
	ReentryFront = "front"
	ReentryBack  = "back"
)

// CookieName and HeaderName carry the ticket in token and naive mode.
const (
	CookieName = "rbq"
	HeaderName = "X-Queue-Ticket"
)

// Config controls the room.
type Config struct {
	Policy       string
	Mode         string
	ReleaseAt    time.Time     // zero means already released
	AdmitRate    float64       // users admitted per second
	Burst        float64       // most admissions that can happen at once
	MaxActive    int           // most users admitted at the same time (0 = no limit)
	SessionTTL   time.Duration // how long an admitted user may reach the backend
	AbandonAfter time.Duration // a queued user not seen for this long is skipped
	PollInterval time.Duration // suggested time between status checks
	Secret       []byte        // HMAC key for token mode
	Seed         uint64        // for the random policy
	// GatePath limits admission to paths with this prefix (e.g. "/result");
	// everything else (the page itself, images) passes straight through.
	// Empty gates every path.
	GatePath string

	// LeaseRequests is the most requests one admission may send to the
	// backend (0 = no limit within SessionTTL). A user whose lease is used up
	// rejoins the queue at the front or the back (Reentry).
	LeaseRequests int
	Reentry       string

	// Adaptive turns on closed-loop admission. Every ControlInterval the room
	// looks at the backend requests that finished in that interval: if more
	// than SlowShare of them took longer than TargetLatency (or failed, or
	// were abandoned by the user), the rate is multiplied by DecreaseFactor;
	// otherwise, if users are waiting, it grows by IncreaseStep. The rate
	// stays within [MinRate, MaxRate]; AdmitRate is the starting value.
	Adaptive        bool
	TargetLatency   time.Duration
	SlowShare       float64
	ControlInterval time.Duration
	IncreaseStep    float64
	DecreaseFactor  float64
	MinRate         float64
	MaxRate         float64
}

const (
	stateQueued = iota
	stateAdmitted
)

type ticket struct {
	id       string
	rank     int64
	state    int
	lastSeen time.Time
	expires  time.Time
	uses     int // backend requests in the current admission
	gen      int // admission number, to match expiry entries
}

// admission is one entry in the expiry list.
type admission struct {
	t   *ticket
	gen int
}

// RatePoint is the admission rate during one control interval.
type RatePoint struct {
	TMs      int64   `json:"t_ms"` // Unix ms at the end of the interval
	Rate     float64 `json:"rate"`
	Finished int64   `json:"finished"`
	Slow     int64   `json:"slow"`
	Queue    int     `json:"queue"`
}

// Room is the waiting room state machine. It is safe for concurrent use.
type Room struct {
	cfg Config
	m   *metrics.Registry
	rng *mrand.Rand

	mu         sync.Mutex
	tickets    map[string]*ticket
	preRelease []*ticket
	queue      []*ticket // ordered by rank; queue[head:] are waiting
	head       int
	nextRank   int64
	released   bool
	tokens     float64
	lastTick   time.Time
	admitted   []admission // in admission order; expiries are in the same order
	active     int

	rate        float64 // current admission rate
	lastControl time.Time
	rateLog     []RatePoint
	finished    atomic.Int64 // backend requests finished in this control interval
	slow        atomic.Int64 // of those, slower than the target, failed or abandoned
	inflight    atomic.Int64 // backend requests in progress
}

// New creates a room. Call Run (or Tick) to make admissions happen.
func New(cfg Config) *Room {
	if cfg.Policy == "" {
		cfg.Policy = PolicyFIFO
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeToken
	}
	if cfg.Burst <= 0 {
		cfg.Burst = max(1, cfg.AdmitRate)
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 60 * time.Second
	}
	if cfg.AbandonAfter == 0 {
		cfg.AbandonAfter = 30 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Reentry == "" {
		cfg.Reentry = ReentryFront
	}
	if cfg.Adaptive {
		if cfg.TargetLatency == 0 {
			cfg.TargetLatency = 500 * time.Millisecond
		}
		if cfg.SlowShare == 0 {
			cfg.SlowShare = 0.1
		}
		if cfg.ControlInterval == 0 {
			cfg.ControlInterval = time.Second
		}
		if cfg.IncreaseStep == 0 {
			cfg.IncreaseStep = 0.05 * cfg.AdmitRate
		}
		if cfg.DecreaseFactor == 0 {
			cfg.DecreaseFactor = 0.8
		}
		if cfg.MinRate == 0 {
			cfg.MinRate = 0.1 * cfg.AdmitRate
		}
		if cfg.MaxRate == 0 {
			cfg.MaxRate = 4 * cfg.AdmitRate
		}
	}
	r := &Room{
		cfg:     cfg,
		m:       metrics.New("wroom"),
		rng:     mrand.New(mrand.NewPCG(cfg.Seed, 0x5EED)),
		tickets: map[string]*ticket{},
		rate:    cfg.AdmitRate,
	}
	if cfg.ReleaseAt.IsZero() {
		r.released, r.lastTick = true, time.Now()
	}
	r.m.GaugeFunc("queue_length", func() int64 {
		r.mu.Lock()
		defer r.mu.Unlock()
		return int64(len(r.queue) - r.head + len(r.preRelease))
	})
	r.m.GaugeFunc("active_sessions", func() int64 { r.mu.Lock(); defer r.mu.Unlock(); return int64(r.active) })
	r.m.GaugeFunc("admit_rate_milli", func() int64 { r.mu.Lock(); defer r.mu.Unlock(); return int64(r.rate * 1000) })
	return r
}

// Rate is the current admission rate (users per second).
func (r *Room) Rate() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rate
}

// RateLog returns the admission rate of every control interval so far.
func (r *Room) RateLog() []RatePoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RatePoint(nil), r.rateLog...)
}

// Observe reports one finished backend request to the controller. slow is
// true if it took longer than the target, failed, or the user gave up on it.
func (r *Room) Observe(d time.Duration, failed bool) {
	r.finished.Add(1)
	if failed || d > r.cfg.TargetLatency {
		r.slow.Add(1)
	}
}

// control adjusts the admission rate once per control interval.
func (r *Room) control(now time.Time) {
	if !r.cfg.Adaptive {
		return
	}
	if r.lastControl.IsZero() {
		r.lastControl = now
		return
	}
	if now.Sub(r.lastControl) < r.cfg.ControlInterval {
		return
	}
	r.lastControl = now
	fin, slow := r.finished.Swap(0), r.slow.Swap(0)
	waiting := len(r.queue) - r.head
	switch {
	case fin == 0 && r.inflight.Load() > 0, // nothing finished: the backend is stuck
		fin > 0 && float64(slow) > r.cfg.SlowShare*float64(fin):
		r.rate *= r.cfg.DecreaseFactor
		r.m.Counter("rate_decreases").Inc()
	case waiting > 0 && (fin > 0 || r.active == 0):
		r.rate += r.cfg.IncreaseStep
		r.m.Counter("rate_increases").Inc()
	}
	r.rate = min(max(r.rate, r.cfg.MinRate), r.cfg.MaxRate)
	r.rateLog = append(r.rateLog, RatePoint{TMs: now.UnixMilli(), Rate: r.rate, Finished: fin, Slow: slow, Queue: waiting})
}

// Metrics exposes the registry.
func (r *Room) Metrics() *metrics.Registry { return r.m }

// Run ticks every interval until stop is closed.
func (r *Room) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			r.Tick(now)
		}
	}
}

// Tick performs release, session expiry and admissions up to now.
func (r *Room) Tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.released {
		if !r.cfg.ReleaseAt.IsZero() && now.Before(r.cfg.ReleaseAt) {
			return
		}
		r.release(now)
	}
	r.expireSessions(now)
	r.control(now)
	dt := now.Sub(r.lastTick).Seconds()
	r.lastTick = now
	burst := r.cfg.Burst
	if r.cfg.Adaptive {
		burst = max(1, r.cfg.Burst*r.rate/r.cfg.AdmitRate)
	}
	r.tokens = min(burst, r.tokens+r.rate*dt)
	for r.tokens >= 1 && r.head < len(r.queue) && (r.cfg.MaxActive == 0 || r.active < r.cfg.MaxActive) {
		t := r.queue[r.head]
		r.queue[r.head] = nil
		r.head++
		if t.state != stateQueued {
			continue
		}
		if now.Sub(t.lastSeen) > r.cfg.AbandonAfter {
			delete(r.tickets, t.id)
			r.m.Counter("abandoned").Inc()
			continue
		}
		r.admit(t, now)
		r.tokens--
	}
	if r.head > 4096 && r.head > len(r.queue)/2 {
		r.queue = append([]*ticket(nil), r.queue[r.head:]...)
		r.head = 0
	}
}

func (r *Room) release(now time.Time) {
	r.released = true
	r.lastTick = now
	pre := r.preRelease
	r.preRelease = nil
	if r.cfg.Policy == PolicyRandom {
		r.rng.Shuffle(len(pre), func(i, j int) { pre[i], pre[j] = pre[j], pre[i] })
		for i, t := range pre {
			t.rank = int64(i)
		}
		r.nextRank = int64(len(pre))
	} else {
		sort.Slice(pre, func(i, j int) bool { return pre[i].rank < pre[j].rank })
	}
	r.queue = append(r.queue, pre...)
	r.m.Counter("prerelease_tickets").Add(int64(len(pre)))
}

func (r *Room) expireSessions(now time.Time) {
	n := 0
	for n < len(r.admitted) {
		a := r.admitted[n]
		if a.t.gen == a.gen && a.t.state == stateAdmitted {
			if now.Before(a.t.expires) {
				break
			}
			delete(r.tickets, a.t.id)
			r.active--
		}
		// Entries of tickets that were re-queued or re-admitted are stale.
		r.admitted[n] = admission{}
		n++
	}
	if n > 0 {
		r.admitted = r.admitted[n:]
	}
}

func (r *Room) admit(t *ticket, now time.Time) {
	t.state = stateAdmitted
	t.expires = now.Add(r.cfg.SessionTTL)
	t.uses = 0
	t.gen++
	r.admitted = append(r.admitted, admission{t: t, gen: t.gen})
	r.active++
	r.m.Counter("admitted").Inc()
	if t.gen > 1 {
		r.m.Counter("readmitted").Inc()
	}
}

// requeue puts an admitted user whose lease is used up back in the queue.
func (r *Room) requeue(t *ticket) {
	t.state = stateQueued
	r.active--
	r.m.Counter("lease_expired").Inc()
	if r.cfg.Reentry == ReentryBack || r.head >= len(r.queue) {
		t.rank = r.nextRank
		r.nextRank++
		r.queue = append(r.queue, t)
		return
	}
	t.rank = r.queue[r.head].rank - 1
	if r.head > 0 {
		r.head--
		r.queue[r.head] = t
		return
	}
	r.queue = append([]*ticket{t}, r.queue...)
}

// Decision is what the room decided for one request.
type Decision struct {
	Admit    bool
	TicketID string
	Position int64         // -1 before release under the random policy
	New      bool          // a ticket was created for this request
	ETA      time.Duration // expected wait at the current rate; -1 if unknown
}

func (r *Room) eta(pos int64, now time.Time) time.Duration {
	if pos < 0 || r.rate <= 0 {
		return -1
	}
	d := time.Duration(float64(pos+1) / r.rate * float64(time.Second))
	if !r.released && !r.cfg.ReleaseAt.IsZero() {
		d += r.cfg.ReleaseAt.Sub(now)
	}
	return d
}

func (r *Room) queued(t *ticket, now time.Time, isNew bool) Decision {
	p := r.position(t)
	return Decision{TicketID: t.id, Position: p, New: isNew, ETA: r.eta(p, now)}
}

// Arrive handles one request that carries ticket id (possibly empty).
func (r *Room) Arrive(id string, now time.Time) Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tickets[id]; ok && id != "" {
		t.lastSeen = now
		switch {
		case t.state == stateAdmitted && now.Before(t.expires):
			if r.cfg.LeaseRequests > 0 && t.uses >= r.cfg.LeaseRequests && r.released {
				r.requeue(t)
				return r.queued(t, now, false)
			}
			t.uses++
			return Decision{Admit: true, TicketID: t.id}
		case t.state == stateQueued:
			return r.queued(t, now, false)
		}
	}
	t := &ticket{id: newID(), lastSeen: now}
	r.tickets[t.id] = t
	r.m.Counter("tickets_created").Inc()
	if !r.released {
		if r.cfg.Policy == PolicyFIFO {
			t.rank = r.nextRank
			r.nextRank++
		}
		r.preRelease = append(r.preRelease, t)
		return r.queued(t, now, true)
	}
	// Fast path: nobody is waiting and there is spare capacity right now.
	if r.head == len(r.queue) && r.tokens >= 1 && (r.cfg.MaxActive == 0 || r.active < r.cfg.MaxActive) {
		r.tokens--
		r.admit(t, now)
		t.uses = 1
		r.m.Counter("fast_path_admits").Inc()
		return Decision{Admit: true, TicketID: t.id, New: true}
	}
	t.rank = r.nextRank
	r.nextRank++
	r.queue = append(r.queue, t)
	return r.queued(t, now, true)
}

func (r *Room) position(t *ticket) int64 {
	if !r.released {
		if r.cfg.Policy == PolicyRandom {
			return -1
		}
		return t.rank
	}
	if r.head >= len(r.queue) {
		return 0
	}
	return max(0, t.rank-r.queue[r.head].rank)
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Handler is the HTTP front of the room. Admitted requests are proxied to
// the backend unchanged.
type Handler struct {
	room    *Room
	proxy   *httputil.ReverseProxy
	backend *url.URL
}

// NewHandler builds the HTTP handler.
func NewHandler(room *Room, backend string) (*Handler, error) {
	u, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = &http.Transport{
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		IdleConnTimeout:     90 * time.Second,
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		if r.Context().Err() != nil {
			// The user gave up while the backend was still working.
			room.m.Counter("client_canceled").Inc()
			return
		}
		room.m.Counter("proxy_errors").Inc()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"bad gateway"}`))
	}
	return &Handler{room: room, proxy: p, backend: u}, nil
}

// Routes returns the mux: /metrics and /stats belong to the room, everything
// else goes through admission.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", h.room.m.ServeMetrics)
	mux.HandleFunc("/stats", h.room.m.ServeStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/ratelog", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.room.RateLog())
	})
	mux.Handle("/", h)
	return mux
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if g := h.room.cfg.GatePath; g != "" && !strings.HasPrefix(req.URL.Path, g) {
		h.proxy.ServeHTTP(w, req)
		return
	}
	id := h.ticketFrom(req)
	d := h.room.Arrive(id, time.Now())
	if h.room.cfg.Mode == ModeToken && d.New {
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: h.sign(d.TicketID), Path: "/", HttpOnly: true})
	}
	if d.Admit {
		h.room.m.Counter("proxied").Inc()
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		h.room.inflight.Add(1)
		h.proxy.ServeHTTP(sw, req)
		h.room.inflight.Add(-1)
		failed := sw.code >= 500 || req.Context().Err() != nil
		h.room.Observe(time.Since(start), failed)
		return
	}
	h.room.m.Counter("queued_responses").Inc()
	jitter := 0.8 + 0.4*mrand.Float64()
	retry := time.Duration(float64(h.room.cfg.PollInterval) * jitter)
	eta := int64(-1)
	if d.ETA >= 0 {
		eta = d.ETA.Milliseconds()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"status":"queued","ticket":%q,"position":%d,"eta_ms":%d,"retry_after_ms":%d}`,
		d.TicketID, d.Position, eta, retry.Milliseconds())
}

// statusWriter remembers the status code the backend answered with.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (h *Handler) ticketFrom(req *http.Request) string {
	if h.room.cfg.Mode == ModeNaive {
		return req.Header.Get(HeaderName)
	}
	c, err := req.Cookie(CookieName)
	if err != nil {
		return ""
	}
	id, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(h.mac(id))) {
		h.room.m.Counter("bad_tokens").Inc()
		return ""
	}
	return id
}

func (h *Handler) sign(id string) string { return id + "." + h.mac(id) }

func (h *Handler) mac(id string) string {
	m := hmac.New(sha256.New, h.room.cfg.Secret)
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
