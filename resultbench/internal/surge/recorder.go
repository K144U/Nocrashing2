package surge

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/k144u/nocrashing/resultbench/internal/oracle"
)

// Request kinds.
const (
	KindFirst   = "first"   // the student's first load of the page
	KindPoll    = "poll"    // automatic reload: before release, or waiting-room polling
	KindRefresh = "refresh" // manual refresh while waiting in the queue
	KindRetry   = "retry"   // reload after an error or timeout
	KindRepeat  = "repeat"  // looking again after already seeing the result
	KindBot     = "bot"     // any request from an automated client
)

type requestRec struct {
	user      int64
	kind      string
	tMs       int64 // send time, ms relative to release
	latencyMs float64
	status    int
	outcome   oracle.Outcome
	target    int
	roll      int64
}

type userRec struct {
	id              int64
	roll            int64
	valid           bool
	arrivalMs       int64 // relative to release
	admittedMs      int64 // first response that was neither queued nor not-released; -1 if none
	finalMs         int64 // when the user saw a final answer; -1 if never
	firstTryMs      int64 // first request sent at or after release; -1 if none
	final           string
	requests        int
	failures        int
	polls           int
	manualRefreshes int
	ticketsLost     int
	gaveUp          string // "", "attempts", "no_retry", "session", "shutdown", "queue", "queue_eta"
	patienceMs      int64
	queuePatienceMs int64 // 0: no limit
	queueWaitMs     int64 // time spent in the queue by a student who gave up there
	bot             bool
	rollsFetched    int // bots: rolls answered with a final result
}

type recorder struct {
	dir      string
	reqF     *os.File
	reqGz    *gzip.Writer
	reqCSV   *csv.Writer
	usrF     *os.File
	usrGz    *gzip.Writer
	usrCSV   *csv.Writer
	reqCh    chan requestRec
	usrCh    chan userRec
	done     chan struct{}
	agg      aggregate
	perSec   map[int64]*secondStats
	deadline []float64
}

// aggregate is computed by the recorder goroutine as records stream in.
type aggregate struct {
	Requests   int64
	ByOutcome  [oracle.NumOutcomes]int64
	ByKind     map[string]int64
	LatCorrect []float32
	LatAll     []float32
	Users      []userRec
	// Bot traffic, also included in the totals above.
	BotRequests int64
	BotBackend  int64
}

type secondStats struct {
	arrivals int64
	sent     int64
	outcomes [oracle.NumOutcomes]int64
	backend  int64 // requests that reached the backend (not gated or queued)
	latSum   float64
	latN     int64
}

func newRecorder(dir string, deadlines []float64) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &recorder{dir: dir, reqCh: make(chan requestRec, 1<<16), usrCh: make(chan userRec, 1<<14),
		done: make(chan struct{}), perSec: map[int64]*secondStats{}, deadline: deadlines}
	r.agg.ByKind = map[string]int64{}
	var err error
	if r.reqF, r.reqGz, r.reqCSV, err = openCSV(filepath.Join(dir, "requests.csv.gz"),
		[]string{"user", "kind", "t_ms", "latency_ms", "status", "outcome", "target", "roll"}); err != nil {
		return nil, err
	}
	if r.usrF, r.usrGz, r.usrCSV, err = openCSV(filepath.Join(dir, "users.csv.gz"),
		[]string{"user", "roll", "valid", "arrival_ms", "admitted_ms", "final_ms", "final", "requests", "failures",
			"polls", "manual_refreshes", "tickets_lost", "gave_up", "patience_ms", "first_try_ms",
			"queue_patience_ms", "queue_wait_ms", "bot", "rolls_fetched"}); err != nil {
		return nil, err
	}
	go r.loop()
	return r, nil
}

func openCSV(path string, header []string) (*os.File, *gzip.Writer, *csv.Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, nil, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	w := csv.NewWriter(bufio.NewWriterSize(gz, 1<<16))
	w.Write(header)
	return f, gz, w, nil
}

func (r *recorder) sec(tMs int64) *secondStats {
	s := tMs / 1000
	if tMs < 0 && tMs%1000 != 0 {
		s--
	}
	st, ok := r.perSec[s]
	if !ok {
		st = &secondStats{}
		r.perSec[s] = st
	}
	return st
}

func (r *recorder) loop() {
	defer close(r.done)
	reqCh, usrCh := r.reqCh, r.usrCh
	for reqCh != nil || usrCh != nil {
		select {
		case q, ok := <-reqCh:
			if !ok {
				reqCh = nil
				continue
			}
			r.reqCSV.Write([]string{
				strconv.FormatInt(q.user, 10), q.kind, strconv.FormatInt(q.tMs, 10),
				strconv.FormatFloat(q.latencyMs, 'f', 2, 64), strconv.Itoa(q.status), q.outcome.String(),
				strconv.Itoa(q.target), strconv.FormatInt(q.roll, 10),
			})
			r.agg.Requests++
			r.agg.ByOutcome[q.outcome]++
			r.agg.ByKind[q.kind]++
			if q.kind == KindBot {
				r.agg.BotRequests++
				if q.outcome != oracle.Queued && q.outcome != oracle.NotReleased {
					r.agg.BotBackend++
				}
			} else {
				r.agg.LatAll = append(r.agg.LatAll, float32(q.latencyMs))
			}
			if q.outcome == oracle.Correct && q.kind != KindBot {
				r.agg.LatCorrect = append(r.agg.LatCorrect, float32(q.latencyMs))
			}
			st := r.sec(q.tMs)
			st.sent++
			st.outcomes[q.outcome]++
			if q.outcome != oracle.Queued && q.outcome != oracle.NotReleased {
				st.backend++
			}
			st.latSum += q.latencyMs
			st.latN++
		case u, ok := <-usrCh:
			if !ok {
				usrCh = nil
				continue
			}
			r.usrCSV.Write([]string{
				strconv.FormatInt(u.id, 10), strconv.FormatInt(u.roll, 10), strconv.FormatBool(u.valid),
				strconv.FormatInt(u.arrivalMs, 10), strconv.FormatInt(u.admittedMs, 10), strconv.FormatInt(u.finalMs, 10),
				u.final, strconv.Itoa(u.requests), strconv.Itoa(u.failures), strconv.Itoa(u.polls),
				strconv.Itoa(u.manualRefreshes), strconv.Itoa(u.ticketsLost), u.gaveUp, strconv.FormatInt(u.patienceMs, 10),
				strconv.FormatInt(u.firstTryMs, 10), strconv.FormatInt(u.queuePatienceMs, 10), strconv.FormatInt(u.queueWaitMs, 10),
				strconv.FormatBool(u.bot), strconv.Itoa(u.rollsFetched),
			})
			r.agg.Users = append(r.agg.Users, u)
			if !u.bot {
				r.sec(u.arrivalMs).arrivals++
			}
		}
	}
}

// close flushes everything and writes timeseries.csv.
func (r *recorder) close(extra func(sec int64) []string, extraHeader []string) error {
	close(r.reqCh)
	close(r.usrCh)
	<-r.done
	for _, c := range []struct {
		w  *csv.Writer
		gz *gzip.Writer
		f  *os.File
	}{{r.reqCSV, r.reqGz, r.reqF}, {r.usrCSV, r.usrGz, r.usrF}} {
		c.w.Flush()
		if err := c.w.Error(); err != nil {
			return err
		}
		c.gz.Close()
		if err := c.f.Close(); err != nil {
			return err
		}
	}
	f, err := os.Create(filepath.Join(r.dir, "timeseries.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	header := []string{"t_s", "arrivals", "sent", "backend", "mean_latency_ms"}
	for i := 0; i < oracle.NumOutcomes; i++ {
		header = append(header, oracle.Outcome(i).String())
	}
	w.Write(append(header, extraHeader...))
	secs := make([]int64, 0, len(r.perSec))
	for s := range r.perSec {
		secs = append(secs, s)
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })
	for _, s := range secs {
		st := r.perSec[s]
		mean := 0.0
		if st.latN > 0 {
			mean = st.latSum / float64(st.latN)
		}
		row := []string{strconv.FormatInt(s, 10), strconv.FormatInt(st.arrivals, 10), strconv.FormatInt(st.sent, 10),
			strconv.FormatInt(st.backend, 10), strconv.FormatFloat(mean, 'f', 2, 64)}
		for _, n := range st.outcomes {
			row = append(row, strconv.FormatInt(n, 10))
		}
		if extra != nil {
			row = append(row, extra(s)...)
		}
		w.Write(row)
	}
	w.Flush()
	return w.Error()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
