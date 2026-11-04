package wroom

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFIFOOrderAndRate(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{Policy: PolicyFIFO, ReleaseAt: t0, AdmitRate: 10, Burst: 1, AbandonAfter: time.Hour})
	var ids []string
	for i := 0; i < 5; i++ {
		d := r.Arrive("", t0.Add(-time.Duration(5-i)*time.Second))
		if d.Admit {
			t.Fatal("admitted before release")
		}
		ids = append(ids, d.TicketID)
	}
	if d := r.Arrive(ids[3], t0.Add(-time.Second)); d.Position != 3 {
		t.Fatalf("pre-release FIFO position = %d, want 3", d.Position)
	}
	r.Tick(t0) // release, no tokens yet
	var order []int
	for step := 1; step <= 10 && len(order) < 5; step++ {
		now := t0.Add(time.Duration(step) * 100 * time.Millisecond)
		r.Tick(now)
		for i, id := range ids {
			d := r.Arrive(id, now)
			if d.Admit && !contains(order, i) {
				order = append(order, i)
			}
		}
	}
	for i := range order {
		if order[i] != i {
			t.Fatalf("FIFO admitted in order %v", order)
		}
	}
	if len(order) != 5 {
		t.Fatalf("admitted %d of 5 after 1s at 10/s", len(order))
	}
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestRandomPolicyShufflesPreQueue(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{Policy: PolicyRandom, ReleaseAt: t0, AdmitRate: 1000, Burst: 1000, AbandonAfter: time.Hour, Seed: 3})
	const n = 200
	ids := make([]string, n)
	for i := range ids {
		ids[i] = r.Arrive("", t0.Add(-time.Minute+time.Duration(i)*time.Millisecond)).TicketID
		if p := r.Arrive(ids[i], t0.Add(-time.Second)).Position; p != -1 {
			t.Fatalf("random policy revealed position %d before release", p)
		}
	}
	r.Tick(t0)
	// Collect ranks: lower rank is admitted first.
	inversions := 0
	for i := 1; i < n; i++ {
		if r.tickets[ids[i]].rank < r.tickets[ids[i-1]].rank {
			inversions++
		}
	}
	if inversions < n/4 {
		t.Fatalf("pre-queue does not look shuffled (%d inversions)", inversions)
	}
	late := r.Arrive("", t0.Add(time.Millisecond))
	if late.Admit || r.tickets[late.TicketID].rank < n {
		t.Fatal("post-release arrival jumped ahead of the pre-queue")
	}
}

func TestAbandonedTicketsSkipped(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 1, Burst: 1, AbandonAfter: 10 * time.Second})
	ghost := r.Arrive("", t0.Add(-time.Minute)).TicketID
	live := r.Arrive("", t0.Add(-time.Second)).TicketID
	r.Tick(t0)
	r.Arrive(live, t0.Add(500*time.Millisecond))
	r.Tick(t0.Add(time.Second))
	if !r.Arrive(live, t0.Add(time.Second)).Admit {
		t.Fatal("live user not admitted after ghost was skipped")
	}
	if r.Metrics().Snapshot()["abandoned"] != 1 {
		t.Fatal("ghost not counted as abandoned")
	}
	_ = ghost
}

func TestSessionExpiry(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 100, Burst: 100, SessionTTL: 5 * time.Second, MaxActive: 1})
	r.Tick(t0)
	r.Tick(t0.Add(time.Second))
	a := r.Arrive("", t0.Add(time.Second))
	if !a.Admit {
		t.Fatal("first user should take the fast path")
	}
	b := r.Arrive("", t0.Add(time.Second))
	if b.Admit {
		t.Fatal("max-active not enforced")
	}
	r.Tick(t0.Add(7 * time.Second))
	if !r.Arrive(b.TicketID, t0.Add(7*time.Second)).Admit {
		t.Fatal("second user not admitted after first session expired")
	}
}

func TestHandlerTokenAndNaive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"roll":1}`)) }))
	defer backend.Close()
	for _, mode := range []string{ModeToken, ModeNaive} {
		room := New(Config{Mode: mode, AdmitRate: 0.0001, Burst: 1, Secret: []byte("k")})
		room.Tick(time.Now())
		h, _ := NewHandler(room, backend.URL)
		srv := httptest.NewServer(h.Routes())
		// First request: fast path, admitted.
		resp, _ := http.Get(srv.URL + "/result?roll=1")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", mode, resp.StatusCode)
		}
		// Second user: no tokens left, queued.
		resp, _ = http.Get(srv.URL + "/result?roll=2")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"status":"queued"`) {
			t.Fatalf("%s: expected queued, got %s", mode, body)
		}
		hasCookie := len(resp.Cookies()) > 0
		if hasCookie != (mode == ModeToken) {
			t.Fatalf("%s: cookie present = %v", mode, hasCookie)
		}
		srv.Close()
	}
}

func TestForgedTokenRejected(t *testing.T) {
	room := New(Config{Mode: ModeToken, AdmitRate: 1, Secret: []byte("k")})
	h, _ := NewHandler(room, "http://127.0.0.1:1")
	req := httptest.NewRequest("GET", "/result?roll=1", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "abc.deadbeef"})
	if id := h.ticketFrom(req); id != "" {
		t.Fatal("forged token accepted")
	}
}

func TestLeaseRequeuesAtFront(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 1, Burst: 1, LeaseRequests: 2, AbandonAfter: time.Hour})
	r.Tick(t0)
	r.Tick(t0.Add(time.Second))
	a := r.Arrive("", t0.Add(time.Second)) // fast path: use 1
	if !a.Admit {
		t.Fatal("first user should take the fast path")
	}
	b := r.Arrive("", t0.Add(time.Second)) // no tokens: queued
	if b.Admit {
		t.Fatal("second user admitted without a token")
	}
	if !r.Arrive(a.TicketID, t0.Add(1100*time.Millisecond)).Admit { // use 2
		t.Fatal("lease ended too early")
	}
	d := r.Arrive(a.TicketID, t0.Add(1200*time.Millisecond)) // use 3: lease used up
	if d.Admit {
		t.Fatal("lease not enforced")
	}
	if d.Position != 0 {
		t.Fatalf("re-queued user at position %d, want 0 (front)", d.Position)
	}
	if bp := r.Arrive(b.TicketID, t0.Add(1200*time.Millisecond)).Position; bp != 1 {
		t.Fatalf("waiting user at position %d, want 1", bp)
	}
	r.Tick(t0.Add(2200 * time.Millisecond))
	if !r.Arrive(a.TicketID, t0.Add(2200*time.Millisecond)).Admit {
		t.Fatal("re-queued user not re-admitted first")
	}
	s := r.Metrics().Snapshot()
	if s["lease_expired"] != 1 || s["readmitted"] != 1 {
		t.Fatalf("lease_expired=%d readmitted=%d", s["lease_expired"], s["readmitted"])
	}
	if r.active != 1 {
		t.Fatalf("active = %d, want 1", r.active)
	}
}

func TestLeaseRequeuesAtBack(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 1, Burst: 1, LeaseRequests: 1, Reentry: ReentryBack, AbandonAfter: time.Hour})
	r.Tick(t0)
	r.Tick(t0.Add(time.Second))
	a := r.Arrive("", t0.Add(time.Second))
	b := r.Arrive("", t0.Add(time.Second))
	if d := r.Arrive(a.TicketID, t0.Add(1100*time.Millisecond)); d.Admit || d.Position != 1 {
		t.Fatalf("back re-entry: admit=%v position=%d, want queued at 1", d.Admit, d.Position)
	}
	r.Tick(t0.Add(2100 * time.Millisecond))
	if !r.Arrive(b.TicketID, t0.Add(2100*time.Millisecond)).Admit {
		t.Fatal("waiting user should go before the re-queued one")
	}
}

func TestStaleExpiryEntryIgnored(t *testing.T) {
	// A user re-admitted after a lease must not be expired by the entry of
	// their first admission.
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 1, Burst: 1, LeaseRequests: 1, SessionTTL: 5 * time.Second, AbandonAfter: time.Hour})
	r.Tick(t0)
	r.Tick(t0.Add(time.Second))
	a := r.Arrive("", t0.Add(time.Second))      // admitted, expires at 6 s
	r.Arrive(a.TicketID, t0.Add(4*time.Second)) // lease used up: re-queued
	r.Tick(t0.Add(5 * time.Second))             // re-admitted, expires at 10 s
	r.Tick(t0.Add(7 * time.Second))             // first entry is due
	if !r.Arrive(a.TicketID, t0.Add(7*time.Second)).Admit {
		t.Fatal("re-admitted user expired by a stale entry")
	}
	if r.active != 1 {
		t.Fatalf("active = %d, want 1", r.active)
	}
}

func TestAdaptiveRate(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 100, Adaptive: true, TargetLatency: 100 * time.Millisecond,
		AbandonAfter: time.Hour, IncreaseStep: 10, DecreaseFactor: 0.5})
	for i := 0; i < 1000; i++ {
		r.Arrive("", t0.Add(-time.Second))
	}
	r.Tick(t0)
	now := t0
	step := func(lat time.Duration, n int) {
		for i := 0; i < n; i++ {
			r.Observe(lat, false)
		}
		now = now.Add(time.Second)
		r.Tick(now)
	}
	step(10*time.Millisecond, 50) // the release tick started the control clock
	if got := r.Rate(); got != 110 {
		t.Fatalf("fast backend: rate %.1f, want 110", got)
	}
	step(time.Second, 50)
	if got := r.Rate(); got != 55 {
		t.Fatalf("slow backend: rate %.1f, want 55", got)
	}
	for i := 0; i < 20; i++ {
		step(time.Second, 50)
	}
	if got := r.Rate(); got != 10 {
		t.Fatalf("rate %.1f should stop at the minimum 10", got)
	}
	if len(r.RateLog()) != 22 {
		t.Fatalf("rate log has %d points, want 22", len(r.RateLog()))
	}
}

func TestAdaptiveStuckBackend(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 100, Adaptive: true, DecreaseFactor: 0.5, AbandonAfter: time.Hour})
	r.Arrive("", t0.Add(-time.Second))
	r.Tick(t0)
	r.inflight.Add(5) // requests in progress, none finished
	r.Tick(t0.Add(time.Second))
	if got := r.Rate(); got != 50 {
		t.Fatalf("stuck backend: rate %.1f, want 50", got)
	}
}

func TestETA(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	r := New(Config{ReleaseAt: t0, AdmitRate: 10, Burst: 1, AbandonAfter: time.Hour})
	var last Decision
	for i := 0; i < 20; i++ {
		last = r.Arrive("", t0.Add(-10*time.Second))
	}
	// Position 19 before release: 10 s to release plus 20 users at 10/s.
	if last.ETA != 12*time.Second {
		t.Fatalf("pre-release ETA %s, want 12s", last.ETA)
	}
	rr := New(Config{Policy: PolicyRandom, ReleaseAt: t0, AdmitRate: 10})
	if d := rr.Arrive("", t0.Add(-time.Second)); d.ETA != -1 {
		t.Fatalf("random policy ETA before release = %s, want unknown", d.ETA)
	}
}

func TestHandlerReportsETAAndLatency(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte(`{"roll":1}`))
	}))
	defer backend.Close()
	room := New(Config{Mode: ModeToken, AdmitRate: 0.0001, Burst: 1, Secret: []byte("k"), Adaptive: true, TargetLatency: 5 * time.Millisecond})
	room.Tick(time.Now())
	room.tokens = 1 // let the first user in
	h, _ := NewHandler(room, backend.URL)
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/result?roll=1")
	resp.Body.Close()
	resp, _ = http.Get(srv.URL + "/result?roll=2")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"eta_ms":`) {
		t.Fatalf("queued response has no eta_ms: %s", body)
	}
	if room.finished.Load() != 1 || room.slow.Load() != 1 {
		t.Fatalf("finished=%d slow=%d, want 1 and 1", room.finished.Load(), room.slow.Load())
	}
	resp, _ = http.Get(srv.URL + "/ratelog")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") && strings.TrimSpace(string(body)) != "null" {
		t.Fatalf("ratelog: %s", body)
	}
}
