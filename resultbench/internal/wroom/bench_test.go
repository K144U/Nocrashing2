package wroom

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// BenchmarkQueuedPoll is the room's own cost of answering one poll from a
// queued user with a token cookie (no network).
func BenchmarkQueuedPoll(b *testing.B) {
	room := New(Config{Mode: ModeToken, AdmitRate: 0.0001, Burst: 1, Secret: []byte("bench-secret"), AbandonAfter: time.Hour})
	room.Tick(time.Now())
	h, _ := NewHandler(room, "http://127.0.0.1:1")
	mux := h.Routes()
	// Fill the queue and take one ticket's cookie.
	var cookie *http.Cookie
	for i := 0; i < 20000; i++ {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/result?roll=1", nil))
		if c := w.Result().Cookies(); len(c) > 0 {
			cookie = c[0]
		}
	}
	req := httptest.NewRequest("GET", "/result?roll=1", nil)
	req.AddCookie(cookie)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
}
