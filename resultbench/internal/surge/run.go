package surge

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/k144u/nocrashing/resultbench/internal/oracle"
	"github.com/k144u/nocrashing/resultbench/internal/wroom"
)

// Generator runs one scenario.
type Generator struct {
	sc      Scenario
	or      oracle.Oracle
	client  *http.Client
	rec     *recorder
	release time.Time
	corrAt  time.Time
	logf    func(string, ...any)

	active   atomic.Int64
	inflight atomic.Int64
	dropped  atomic.Int64
	localErr atomic.Int64 // errors caused by the generator's own machine

	netErrMu  sync.Mutex
	netErrors map[string]int64
	nextID    atomic.Int64

	samplesMu sync.Mutex
	samples   map[int64][3]float64 // second -> active users, in-flight, generator CPU cores
}

// Summary is written to summary.json at the end of a run.
type Summary struct {
	Scenario          Scenario           `json:"scenario"`
	StartedUnixMs     int64              `json:"started_unix_ms"`
	ReleaseAtUnixMs   int64              `json:"release_at_unix_ms"`
	EndedUnixMs       int64              `json:"ended_unix_ms"`
	ExpectedUsers     float64            `json:"expected_users"`
	Users             int                `json:"users"`
	DroppedUsers      int64              `json:"dropped_users"`
	NetErrors         map[string]int64   `json:"net_errors"`
	UnfinishedUsers   int                `json:"unfinished_users"`
	Requests          int64              `json:"requests"`
	BackendRequests   int64              `json:"backend_requests"`
	RequestsByOutcome map[string]int64   `json:"requests_by_outcome"`
	RequestsByKind    map[string]int64   `json:"requests_by_kind"`
	Amplification     float64            `json:"amplification"`
	BackendAmplif     float64            `json:"backend_amplification"`
	UserSuccessWithin map[string]float64 `json:"user_success_within"`
	UserServedOK      float64            `json:"user_served_ok"`
	UserFinal         map[string]int     `json:"user_final"`
	UserGaveUp        map[string]int     `json:"user_gave_up"`
	WrongRequests     int64              `json:"wrong_requests"`
	StaleRequests     int64              `json:"stale_requests"`
	MissingRequests   int64              `json:"missing_requests"`
	SoftFailures      int64              `json:"soft_failures"`
	LatencyAllMs      Percentiles        `json:"latency_all_ms"`
	LatencyCorrectMs  Percentiles        `json:"latency_correct_ms"`
	TimeToResultMs    Percentiles        `json:"time_to_result_ms"`
	AdmissionWaitMs   Percentiles        `json:"admission_wait_ms"`
	// Students who gave up in a waiting room's queue, and how long they had
	// waited there (time wasted without a result).
	QueueGaveUp       int            `json:"queue_gave_up"`
	QueueGaveUpWaitMs Percentiles    `json:"queue_gave_up_wait_ms"`
	Bots              BotStats       `json:"bots"`
	Generator         GeneratorStats `json:"generator"`
}

// BotStats summarises automated clients. Requests, BackendRequests and
// RequestsByOutcome in Summary include them; every per-student metric does not.
type BotStats struct {
	Users           int     `json:"users"`
	Requests        int64   `json:"requests"`
	BackendRequests int64   `json:"backend_requests"`
	RollsWanted     int     `json:"rolls_wanted"`
	RollsFetched    int     `json:"rolls_fetched"`
	FetchedShare    float64 `json:"fetched_share"`
}

// Percentiles of a distribution.
type Percentiles struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// GeneratorStats lets the analysis discard runs where the load generator,
// not the system under test, was the bottleneck.
type GeneratorStats struct {
	GoMaxProcs   int     `json:"gomaxprocs"`
	CPUMaxCores  float64 `json:"cpu_max_cores"`
	CPUMeanCores float64 `json:"cpu_mean_cores"`
	Saturated    bool    `json:"saturated"`
	LocalErrors  int64   `json:"local_errors"`
}

// New prepares a generator writing into dir.
func New(sc Scenario, dir string, logf func(string, ...any)) (*Generator, error) {
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	rec, err := newRecorder(dir, sc.DeadlinesS)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: 20_000,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Generator{
		sc: sc, or: oracle.Oracle{Cfg: sc.Dataset.Config(), RollOnly: sc.Oracle == "roll_only"}, rec: rec, logf: logf,
		client:  &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		samples: map[int64][3]float64{}, netErrors: map[string]int64{},
	}, nil
}

func (g *Generator) relMs(t time.Time) int64 { return t.Sub(g.release).Milliseconds() }

// Run executes the scenario and writes summary.json. It returns the summary.
func (g *Generator) Run(ctx context.Context) (Summary, error) {
	sc := g.sc
	started := time.Now()
	if sc.ReleaseAtUnixMs > 0 {
		g.release = time.UnixMilli(sc.ReleaseAtUnixMs)
	} else {
		g.release = started.Add(seconds(sc.ReleaseInS))
	}
	g.corrAt = g.release.Add(seconds(sc.Corrections.AtS))
	g.logf("release at %s (in %.1fs), expecting about %.0f students",
		g.release.Format(time.RFC3339Nano), time.Until(g.release).Seconds(), sc.Surge.ExpectedUsers())

	sampleStop := make(chan struct{})
	sampleDone := make(chan GeneratorStats)
	go g.sample(sampleStop, sampleDone)

	userCtx, cancelUsers := context.WithCancel(ctx)
	defer cancelUsers()
	var wg sync.WaitGroup
	g.spawnBots(userCtx, &wg)
	g.spawn(ctx, userCtx, &wg)

	// Let students who are still trying finish, up to the drain limit.
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(seconds(sc.Generator.DrainS)):
		g.logf("drain limit reached with %d students still active; stopping them", g.active.Load())
		cancelUsers()
		<-drained
	case <-ctx.Done():
		cancelUsers()
		<-drained
	}
	close(sampleStop)
	gen := <-sampleDone

	g.samplesMu.Lock()
	samples := g.samples
	g.samplesMu.Unlock()
	if err := g.rec.close(func(s int64) []string {
		v := samples[s]
		return []string{strconv.FormatFloat(v[0], 'f', 0, 64), strconv.FormatFloat(v[1], 'f', 0, 64), strconv.FormatFloat(v[2], 'f', 2, 64)}
	}, []string{"active_users", "inflight", "generator_cpu_cores"}); err != nil {
		return Summary{}, err
	}
	sum := g.summarize(started, gen)
	return sum, writeJSON(filepath.Join(g.rec.dir, "summary.json"), sum)
}

// spawn generates arrivals following the surge curve until it ends.
func (g *Generator) spawn(ctx, userCtx context.Context, wg *sync.WaitGroup) {
	sc := g.sc
	keys := newKeyChooser(sc.Keys, sc.Dataset.N, sc.Dataset.RollBase, sc.Generator.Seed)
	rng := rand.New(rand.NewPCG(sc.Generator.Seed, 0xA77))
	const tick = 10 * time.Millisecond
	startRel := sc.Surge.Start()
	first := g.release.Add(seconds(startRel))
	if d := time.Until(first); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	last := time.Now()
	carry := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			t := now.Sub(g.release).Seconds()
			if t > sc.Surge.PostReleaseS {
				return
			}
			dt := now.Sub(last).Seconds()
			last = now
			expected := sc.Surge.Rate(t) * dt
			var n int
			if sc.Surge.Arrivals == "even" {
				carry += expected
				n = int(carry)
				carry -= float64(n)
			} else {
				n = poisson(rng, expected)
			}
			for i := 0; i < n; i++ {
				roll := keys.Next()
				if sc.Generator.MaxUsers > 0 && g.active.Load() >= int64(sc.Generator.MaxUsers) {
					g.dropped.Add(1)
					continue
				}
				id := g.nextID.Add(1)
				g.active.Add(1)
				wg.Add(1)
				// Spread arrivals within the tick so they are not synchronised.
				offset := time.Duration(rng.Int64N(int64(tick)))
				if sc.Surge.Arrivals == "even" {
					offset = time.Duration(i) * tick / time.Duration(n)
				}
				go func() {
					defer wg.Done()
					defer g.active.Add(-1)
					g.user(userCtx, id, roll, now.Add(offset))
				}()
			}
		}
	}
}

func poisson(r *rand.Rand, mean float64) int {
	if mean <= 0 {
		return 0
	}
	if mean > 50 {
		return max(0, int(math.Round(r.NormFloat64()*math.Sqrt(mean)+mean)))
	}
	l, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func uniform(r *rand.Rand, rg Range) float64 { return rg[0] + r.Float64()*(rg[1]-rg[0]) }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// user is the behaviour of one student, from arrival until they see a
// result, give up, or the run ends.
func (g *Generator) user(ctx context.Context, id, roll int64, arrival time.Time) {
	sc := g.sc
	us := sc.Users
	rng := rand.New(rand.NewPCG(sc.Generator.Seed^uint64(id), 0x05E7))
	if !sleepCtx(ctx, time.Until(arrival)) {
		return
	}
	_, valid := g.or.Cfg.Valid(roll)
	patience := seconds(uniform(rng, us.PatienceS))
	var queuePatience time.Duration
	if us.QueuePatienceS[1] > 0 {
		queuePatience = seconds(uniform(rng, us.QueuePatienceS))
	}
	var queueStart time.Time // when the student's time in the queue started counting
	u := userRec{id: id, roll: roll, valid: valid, arrivalMs: g.relMs(arrival), admittedMs: -1, finalMs: -1, firstTryMs: -1,
		patienceMs: patience.Milliseconds(), queuePatienceMs: queuePatience.Milliseconds()}
	defer func() { g.rec.usrCh <- u }()

	target := rng.IntN(len(sc.Targets))
	var cookie, ticket string
	kind := KindFirst
	deadline := arrival.Add(seconds(us.MaxSessionS))
	for {
		if ctx.Err() != nil {
			u.gaveUp = "shutdown"
			return
		}
		if time.Now().After(deadline) {
			u.gaveUp = "session"
			return
		}
		out, st, newCookie := g.request(ctx, &u, kind, target, roll, cookie, ticket, patience)
		if newCookie != "" {
			cookie = newCookie
		}
		if out != oracle.Queued && out != oracle.NotReleased && u.admittedMs < 0 && !(out == oracle.Timeout || out == oracle.NetError) {
			u.admittedMs = g.relMs(time.Now())
		}
		switch {
		case out.Final():
			u.final = out.String()
			u.finalMs = g.relMs(time.Now())
			if rng.Float64() < us.RepeatViewProb && sleepCtx(ctx, seconds(uniform(rng, us.RepeatAfterS))) {
				g.request(ctx, &u, KindRepeat, target, roll, cookie, ticket, patience)
			}
			return
		case out == oracle.NotReleased:
			u.polls++
			kind = KindPoll
			if !sleepCtx(ctx, seconds(uniform(rng, us.PreReleasePollS))) {
				u.gaveUp = "shutdown"
				return
			}
		case out == oracle.Queued:
			u.polls++
			kind = KindPoll
			if st != nil && st.Ticket != "" {
				ticket = st.Ticket
			}
			now := time.Now()
			if queueStart.IsZero() {
				queueStart = now
				if now.Before(g.release) {
					queueStart = g.release
				}
			}
			waited := max(0, now.Sub(queueStart))
			if queuePatience > 0 {
				if waited >= queuePatience {
					u.gaveUp, u.queueWaitMs = "queue", waited.Milliseconds()
					return
				}
				if us.EtaBalk > 0 && st != nil && st.EtaMs != nil && *st.EtaMs >= 0 {
					eta := time.Duration(*st.EtaMs) * time.Millisecond
					if waited+eta > time.Duration(us.EtaBalk*float64(queuePatience)) {
						u.gaveUp, u.queueWaitMs = "queue_eta", waited.Milliseconds()
						return
					}
				}
			}
			wait := 5 * time.Second
			if st != nil && st.RetryAfterMs > 0 {
				wait = time.Duration(st.RetryAfterMs) * time.Millisecond
			}
			wait = min(max(wait, 200*time.Millisecond), 60*time.Second)
			if queuePatience > 0 && now.Add(wait).After(queueStart.Add(queuePatience)) {
				// Patience runs out before the next poll: wait until then and leave.
				if !sleepCtx(ctx, queueStart.Add(queuePatience).Sub(now)) {
					u.gaveUp = "shutdown"
					return
				}
				u.gaveUp, u.queueWaitMs = "queue", queuePatience.Milliseconds()
				return
			}
			// Manual refresh during the wait (a Poisson event).
			pRefresh := 1 - math.Exp(-us.QueueManualRefreshPerMin*wait.Minutes())
			if rng.Float64() < pRefresh {
				wait = time.Duration(rng.Float64() * float64(wait))
				u.manualRefreshes++
				kind = KindRefresh
				if sc.WaitingRoom.Mode == wroom.ModeNaive && ticket != "" {
					ticket = "" // the page state, and with it the ticket, is gone
					u.ticketsLost++
				}
			}
			if !sleepCtx(ctx, wait) {
				u.gaveUp = "shutdown"
				return
			}
		default: // a failure
			u.failures++
			if u.failures >= us.MaxAttempts {
				u.gaveUp = "attempts"
				u.final = out.String()
				return
			}
			p := us.RetryProb
			if out == oracle.Timeout {
				p = us.ImpatientRetryProb
			}
			if rng.Float64() >= p {
				u.gaveUp = "no_retry"
				u.final = out.String()
				return
			}
			kind = KindRetry
			if len(sc.Targets) > 1 && rng.Float64() < sc.MirrorSwitchProb {
				target = (target + 1 + rng.IntN(len(sc.Targets)-1)) % len(sc.Targets)
			}
			if !sleepCtx(ctx, seconds(uniform(rng, us.RetryDelayS))) {
				u.gaveUp = "shutdown"
				return
			}
		}
	}
}

// spawnBots starts the automated clients described in Scenario.Bots.
func (g *Generator) spawnBots(ctx context.Context, wg *sync.WaitGroup) {
	bs := g.sc.Bots
	if bs.Count == 0 {
		return
	}
	n := int64(g.sc.Dataset.N)
	rng := rand.New(rand.NewPCG(g.sc.Generator.Seed, 0xB07))
	for i := 0; i < bs.Count; i++ {
		start := g.release.Add(seconds(bs.StartS + bs.SpreadS*float64(i)/float64(bs.Count)))
		first := g.sc.Dataset.RollBase + rng.Int64N(n)
		id := g.nextID.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.bot(ctx, id, first, start)
		}()
	}
}

// bot fetches bs.RollsEach consecutive roll numbers, polling and retrying
// at a fixed pace, and never gives up before the run ends.
func (g *Generator) bot(ctx context.Context, id, first int64, start time.Time) {
	bs := g.sc.Bots
	if !sleepCtx(ctx, time.Until(start)) {
		return
	}
	u := userRec{id: id, roll: first, bot: true, arrivalMs: g.relMs(start), admittedMs: -1, finalMs: -1, firstTryMs: -1,
		patienceMs: int64(bs.TimeoutS * 1000)}
	defer func() { g.rec.usrCh <- u }()
	n := int64(g.sc.Dataset.N)
	base := g.sc.Dataset.RollBase
	timeout := seconds(bs.TimeoutS)
	var cookie, ticket string
	for i := 0; i < bs.RollsEach; {
		if ctx.Err() != nil {
			u.gaveUp = "shutdown"
			return
		}
		roll := base + (first-base+int64(i))%n
		c := cookie
		if !bs.KeepCookie {
			c = ""
		}
		out, st, newCookie := g.request(ctx, &u, KindBot, 0, roll, c, ticket, timeout)
		if newCookie != "" {
			cookie = newCookie
		}
		pause := seconds(bs.PollS)
		switch {
		case out == oracle.Queued:
			if st != nil && st.Ticket != "" {
				ticket = st.Ticket
			}
		case out.Final():
			if u.admittedMs < 0 {
				u.admittedMs = g.relMs(time.Now())
			}
			u.rollsFetched++
			u.finalMs = g.relMs(time.Now())
			i++
			pause = seconds(bs.GapS)
		case out != oracle.NotReleased:
			u.failures++
		}
		if !sleepCtx(ctx, pause) {
			u.gaveUp = "shutdown"
			return
		}
	}
}

// request sends one GET /result and classifies the answer.
func (g *Generator) request(ctx context.Context, u *userRec, kind string, target int, roll int64,
	cookie, ticket string, patience time.Duration) (oracle.Outcome, *oracle.Status, string) {
	sc := g.sc
	url := sc.Targets[target] + "/result?roll=" + strconv.FormatInt(roll, 10)
	rctx, cancel := context.WithTimeout(ctx, patience)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if cookie != "" {
		req.Header.Set("Cookie", wroom.CookieName+"="+cookie)
	}
	if ticket != "" {
		req.Header.Set(wroom.HeaderName, ticket)
	}
	sent := time.Now()
	if u.firstTryMs < 0 && !sent.Before(g.release) {
		u.firstTryMs = g.relMs(sent)
	}
	corrPublished := sc.Corrections.Enabled && !sent.Before(g.corrAt)
	g.inflight.Add(1)
	resp, err := g.client.Do(req)
	var out oracle.Outcome
	var st *oracle.Status
	var newCookie string
	status := 0
	if err == nil {
		status = resp.StatusCode
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(sc.Generator.MaxBodyBytes)))
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == wroom.CookieName {
				newCookie = c.Value
			}
		}
		if rerr != nil {
			err = rerr
		} else {
			out, st = g.or.Classify(status, body, roll, corrPublished)
		}
	}
	g.inflight.Add(-1)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			out = oracle.Timeout // run is shutting down; recorded but the user is marked "shutdown"
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(rctx.Err(), context.DeadlineExceeded):
			out = oracle.Timeout
		default:
			out = oracle.NetError
			g.noteNetError(err)
		}
	}
	lat := time.Since(sent)
	u.requests++
	g.rec.reqCh <- requestRec{user: u.id, kind: kind, tMs: g.relMs(sent), latencyMs: float64(lat.Microseconds()) / 1000,
		status: status, outcome: out, target: target, roll: roll}
	return out, st, newCookie
}

// sample records active users, in-flight requests and generator CPU each second.
func (g *Generator) sample(stop <-chan struct{}, done chan<- GeneratorStats) {
	stats := GeneratorStats{GoMaxProcs: runtime.GOMAXPROCS(0)}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastCPU, lastT := cpuTime(), time.Now()
	sum, n := 0.0, 0
	for {
		select {
		case <-stop:
			if n > 0 {
				stats.CPUMeanCores = sum / float64(n)
			}
			stats.Saturated = stats.CPUMaxCores > 0.9*float64(stats.GoMaxProcs)
			done <- stats
			return
		case now := <-t.C:
			cpu := cpuTime()
			cores := (cpu - lastCPU).Seconds() / now.Sub(lastT).Seconds()
			lastCPU, lastT = cpu, now
			stats.CPUMaxCores = math.Max(stats.CPUMaxCores, cores)
			sum += cores
			n++
			s := g.relMs(now) / 1000
			if g.relMs(now) < 0 {
				s--
			}
			g.samplesMu.Lock()
			g.samples[s] = [3]float64{float64(g.active.Load()), float64(g.inflight.Load()), cores}
			g.samplesMu.Unlock()
		}
	}
}

// noteNetError counts network errors by kind. Running out of local ports or
// file descriptors is a fault of the load generator, not of the system under
// test, so such runs are marked as saturated.
func (g *Generator) noteNetError(err error) {
	msg := err.Error()
	kind := "other"
	switch {
	case strings.Contains(msg, "too many open files"):
		kind = "local_too_many_open_files"
	case strings.Contains(msg, "cannot assign requested address"):
		kind = "local_no_free_ports"
	case strings.Contains(msg, "connection refused"):
		kind = "connection_refused"
	case strings.Contains(msg, "connection reset"):
		kind = "connection_reset"
	case strings.Contains(msg, "EOF"):
		kind = "eof"
	case strings.Contains(msg, "i/o timeout"):
		kind = "dial_timeout"
	}
	if strings.HasPrefix(kind, "local_") {
		g.localErr.Add(1)
	}
	g.netErrMu.Lock()
	g.netErrors[kind]++
	g.netErrMu.Unlock()
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func (g *Generator) summarize(started time.Time, gen GeneratorStats) Summary {
	a := &g.rec.agg
	sc := g.sc
	s := Summary{
		Scenario: sc, StartedUnixMs: started.UnixMilli(), ReleaseAtUnixMs: g.release.UnixMilli(), EndedUnixMs: time.Now().UnixMilli(),
		ExpectedUsers: sc.Surge.ExpectedUsers(), Users: len(a.Users), DroppedUsers: g.dropped.Load(), Requests: a.Requests,
		RequestsByOutcome: map[string]int64{}, RequestsByKind: a.ByKind, UserSuccessWithin: map[string]float64{},
		UserFinal: map[string]int{}, UserGaveUp: map[string]int{}, Generator: gen, NetErrors: g.netErrors,
		WrongRequests: a.ByOutcome[oracle.Wrong], StaleRequests: a.ByOutcome[oracle.Stale],
		MissingRequests: a.ByOutcome[oracle.Missing], SoftFailures: a.ByOutcome[oracle.SoftFailure],
		LatencyAllMs: percentiles(a.LatAll), LatencyCorrectMs: percentiles(a.LatCorrect),
	}
	if g.localErr.Load() > 0 {
		s.Generator.Saturated = true
		s.Generator.LocalErrors = g.localErr.Load()
	}
	for i, n := range a.ByOutcome {
		if n > 0 {
			s.RequestsByOutcome[oracle.Outcome(i).String()] = n
		}
	}
	s.BackendRequests = a.Requests - a.ByOutcome[oracle.Queued] - a.ByOutcome[oracle.NotReleased]
	students := make([]userRec, 0, len(a.Users))
	for _, u := range a.Users {
		if u.bot {
			s.Bots.Users++
			s.Bots.RollsFetched += u.rollsFetched
			continue
		}
		students = append(students, u)
	}
	s.Users = len(students)
	s.Bots.Requests, s.Bots.BackendRequests = a.BotRequests, a.BotBackend
	s.Bots.RollsWanted = s.Bots.Users * sc.Bots.RollsEach
	if s.Bots.RollsWanted > 0 {
		s.Bots.FetchedShare = float64(s.Bots.RollsFetched) / float64(s.Bots.RollsWanted)
	}
	if len(students) > 0 {
		s.Amplification = float64(a.Requests-a.BotRequests) / float64(len(students))
		s.BackendAmplif = float64(s.BackendRequests-a.BotBackend) / float64(len(students))
	}
	var ttr, wait, qwait []float32
	within := make([]int, len(sc.DeadlinesS))
	ok := 0
	for _, u := range students {
		final := u.final
		if final == "" {
			final = "none"
		}
		s.UserFinal[final]++
		if u.gaveUp != "" {
			s.UserGaveUp[u.gaveUp]++
		}
		if u.gaveUp == "shutdown" {
			s.UnfinishedUsers++
		}
		// Deadlines count from the student's first request after release:
		// a student who arrived early only sees the result at their next
		// reload, which is not the portal's doing.
		startMs := max(u.arrivalMs, 0)
		if u.firstTryMs >= 0 {
			startMs = u.firstTryMs
		}
		if u.admittedMs >= 0 {
			wait = append(wait, float32(u.admittedMs-startMs))
		}
		if u.gaveUp == "queue" || u.gaveUp == "queue_eta" {
			s.QueueGaveUp++
			qwait = append(qwait, float32(u.queueWaitMs))
		}
		if final == oracle.Correct.String() || final == oracle.CorrectNotFound.String() {
			ok++
			d := float64(u.finalMs - startMs)
			ttr = append(ttr, float32(d))
			for k, dl := range sc.DeadlinesS {
				if d <= dl*1000 {
					within[k]++
				}
			}
		}
	}
	if len(students) > 0 {
		s.UserServedOK = float64(ok) / float64(len(students))
		for k, dl := range sc.DeadlinesS {
			s.UserSuccessWithin[strconv.FormatFloat(dl, 'f', -1, 64)+"s"] = float64(within[k]) / float64(len(students))
		}
	}
	s.TimeToResultMs = percentiles(ttr)
	s.AdmissionWaitMs = percentiles(wait)
	s.QueueGaveUpWaitMs = percentiles(qwait)
	return s
}

func percentiles(v []float32) Percentiles {
	if len(v) == 0 {
		return Percentiles{}
	}
	c := append([]float32(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	at := func(q float64) float64 { return float64(c[min(len(c)-1, int(q*float64(len(c))))]) }
	return Percentiles{N: len(c), P50: at(0.5), P95: at(0.95), P99: at(0.99), Max: float64(c[len(c)-1])}
}
