package surge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/k144u/nocrashing/resultbench/internal/wroom"
)

// TestBotsAndQueuePatience runs students and bots through a real waiting room
// in front of a stub backend and checks that bots are kept out of student
// metrics, that bots finish their rolls, and that impatient students leave
// the queue.
func TestBotsAndQueuePatience(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end run")
	}
	sum := runThroughRoom(t, 0)
	if sum.Bots.Users != 3 {
		t.Fatalf("bots = %d, want 3", sum.Bots.Users)
	}
	if sum.Users < 150 || sum.Users > 250 {
		t.Fatalf("students = %d, want about 200", sum.Users)
	}
	if sum.Bots.RollsFetched != 12 {
		t.Fatalf("bots fetched %d rolls, want 12", sum.Bots.RollsFetched)
	}
	if sum.Bots.Requests == 0 || sum.RequestsByKind[KindBot] != sum.Bots.Requests {
		t.Fatalf("bot requests %d, by kind %d", sum.Bots.Requests, sum.RequestsByKind[KindBot])
	}
	// 200 students at 20 admissions/s need 10 s; with 1-1.5 s of queue
	// patience most must leave the queue.
	if sum.QueueGaveUp < sum.Users/2 {
		t.Fatalf("only %d of %d students left the queue", sum.QueueGaveUp, sum.Users)
	}
	if sum.QueueGaveUpWaitMs.Max > 1600 {
		t.Fatalf("a student waited %.0f ms in the queue with at most 1.5 s of patience", sum.QueueGaveUpWaitMs.Max)
	}
	if sum.WrongRequests != 0 {
		t.Fatalf("%d wrong answers", sum.WrongRequests)
	}
}

// TestETABalking: with a wait estimate, students who cannot make it leave at
// once instead of waiting out their patience.
func TestETABalking(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end run")
	}
	blind := runThroughRoom(t, 0)
	informed := runThroughRoom(t, 1)
	if informed.UserGaveUp["queue_eta"] == 0 {
		t.Fatal("no student left on seeing the estimate")
	}
	if informed.QueueGaveUpWaitMs.P50 >= blind.QueueGaveUpWaitMs.P50 {
		t.Fatalf("median wasted wait %.0f ms with estimates, %.0f ms without",
			informed.QueueGaveUpWaitMs.P50, blind.QueueGaveUpWaitMs.P50)
	}
}

func runThroughRoom(t *testing.T, etaBalk float64) Summary {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"roll":%s}`, r.URL.Query().Get("roll"))
	}))
	defer backend.Close()

	sc := Default()
	sc.Oracle = "roll_only"
	sc.ReleaseInS = 0.5
	sc.Dataset.N = 10_000
	sc.Surge = SurgeSpec{Shape: "constant", PeakUsersPerS: 100, PostReleaseS: 2, Arrivals: "even"}
	sc.Keys.TypoRate = 0
	sc.Users.QueuePatienceS = Range{1, 1.5}
	sc.Users.EtaBalk = etaBalk
	sc.Users.RepeatViewProb = 0
	sc.Generator.DrainS = 10
	sc.Bots = BotSpec{Count: 3, StartS: 0, RollsEach: 4, PollS: 0.05, TimeoutS: 2, KeepCookie: true}

	room := wroom.New(wroom.Config{Mode: wroom.ModeToken, AdmitRate: 20, Burst: 1, Secret: []byte("test-secret"),
		ReleaseAt: time.Now().Add(500 * time.Millisecond), PollInterval: 200 * time.Millisecond, LeaseRequests: 2})
	stop := make(chan struct{})
	defer close(stop)
	go room.Run(5*time.Millisecond, stop)
	h, err := wroom.NewHandler(room, backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	sc.Targets = []string{srv.URL}

	g, err := New(sc, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sum, err := g.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}
