package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/k144u/nocrashing/resultbench/internal/db"
	"github.com/k144u/nocrashing/resultbench/internal/portal"
	"github.com/k144u/nocrashing/resultbench/internal/wroom"
)

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func serve(ctx context.Context, srv *http.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("listening on %s", srv.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	}
}

func newRedis(addr string, poolSize int) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr, PoolSize: poolSize, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second})
}

func runPortal(args []string) error {
	fs := flag.NewFlagSet("portal", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "listen address")
	design := fs.String("design", portal.DesignDB, "db | cache | kv")
	dsn := fs.String("dsn", "postgres://rb:rb@postgres:5432/rb", "PostgreSQL DSN (db, cache)")
	maxConns := fs.Int("db-max-conns", 16, "database connection pool size")
	redisAddr := fs.String("redis", "redis:6379", "Redis address (cache, kv)")
	redisPool := fs.Int("redis-pool", 64, "Redis connection pool size")
	release := fs.String("release-at", "", "release time, Unix ms (empty: already released)")
	ttl := fs.Duration("cache-ttl", 10*time.Minute, "cache entry lifetime (cache)")
	jitter := fs.Float64("ttl-jitter", 0, "spread cache expiry by +- this fraction (cache)")
	coalesce := fs.Bool("coalesce", false, "one database load per key in flight (cache)")
	xfetch := fs.Bool("xfetch", false, "probabilistic early recomputation (cache)")
	beta := fs.Float64("xfetch-beta", 1, "XFetch beta")
	cancelOnDisc := fs.Bool("cancel-on-disconnect", false, "stop database work when the client disconnects")
	workTimeout := fs.Duration("work-timeout", 30*time.Second, "maximum time spent on one request")
	maxWorkers := fs.Int("max-workers", 256, "requests processed at once (0: no limit)")
	maxQueue := fs.Int("max-queue", 4096, "requests waiting for a worker before answering 503 (0: no limit)")
	pageCost := fs.Duration("page-cost", 0, "CPU time to build one page from the database (models framework and rendering overhead)")
	if err := parse(fs, args); err != nil {
		return err
	}
	releaseAt, err := epochMs(*release)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	cfg := portal.Config{
		Design: *design, ReleaseAt: releaseAt, CacheTTL: *ttl, TTLJitter: *jitter, Coalesce: *coalesce,
		XFetch: *xfetch, XFetchBeta: *beta, CancelOnDisconnect: *cancelOnDisc, WorkTimeout: *workTimeout,
		MaxWorkers: *maxWorkers, MaxQueue: *maxQueue,
	}
	if *design != portal.DesignKV {
		cfg.PageCost = portal.NewPageCost(*pageCost)
		log.Printf("page cost %s = %d hash iterations", *pageCost, cfg.PageCost.Iterations())
	}
	if *design != portal.DesignKV {
		pool, err := db.Connect(ctx, *dsn, *maxConns)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := db.WaitReady(ctx, pool, 2*time.Minute); err != nil {
			return err
		}
		cfg.DB = pool
	}
	if *design != portal.DesignDB {
		cfg.Redis = newRedis(*redisAddr, *redisPool)
		defer cfg.Redis.Close()
	}
	s := portal.New(cfg)
	log.Printf("portal design=%s release_at=%v coalesce=%v xfetch=%v jitter=%.2f ttl=%s",
		*design, releaseAt, *coalesce, *xfetch, *jitter, *ttl)
	return serve(ctx, newHTTPServer(*listen, s.Routes()))
}

func runWaitingRoom(args []string) error {
	fs := flag.NewFlagSet("waitingroom", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "listen address")
	backend := fs.String("backend", "http://portal:8080", "backend base URL")
	policy := fs.String("policy", wroom.PolicyFIFO, "fifo | random")
	mode := fs.String("mode", wroom.ModeToken, "naive | token")
	release := fs.String("release-at", "", "release time, Unix ms")
	rate := fs.Float64("admit-rate", 100, "users admitted per second")
	burst := fs.Float64("burst", 0, "admission burst (default: one second of admit-rate)")
	maxActive := fs.Int("max-active", 0, "most admitted users at once (0: no limit)")
	session := fs.Duration("session-ttl", 60*time.Second, "how long an admitted user may use the backend")
	abandon := fs.Duration("abandon-after", 30*time.Second, "skip queued users not seen for this long")
	poll := fs.Duration("poll", 5*time.Second, "suggested time between status checks")
	secret := fs.String("secret", "", "HMAC secret for token mode (required)")
	seed := fs.Uint64("seed", 1, "seed for the random policy")
	gate := fs.String("gate-path", "/result", "only requests under this path go through admission (empty: all)")
	lease := fs.Int("lease-requests", 0, "most backend requests per admission; then the user rejoins the queue (0: no limit)")
	reentry := fs.String("reentry", wroom.ReentryFront, "where a user whose lease is used up rejoins: front | back")
	adaptive := fs.Bool("adaptive", false, "adjust the admission rate from backend response times")
	target := fs.Duration("target-latency", 500*time.Millisecond, "adaptive: backend response time to stay under")
	slowShare := fs.Float64("slow-share", 0.1, "adaptive: share of slow responses that triggers a decrease")
	ctlInterval := fs.Duration("control-interval", time.Second, "adaptive: how often the rate is adjusted")
	incStep := fs.Float64("increase-step", 0, "adaptive: rate increase per interval (default 5% of admit-rate)")
	decFactor := fs.Float64("decrease-factor", 0.8, "adaptive: rate multiplier on a decrease")
	minRate := fs.Float64("min-rate", 0, "adaptive: lowest rate (default 10% of admit-rate)")
	maxRate := fs.Float64("max-rate", 0, "adaptive: highest rate (default 4x admit-rate)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *reentry != wroom.ReentryFront && *reentry != wroom.ReentryBack {
		return fmt.Errorf("-reentry must be front or back")
	}
	if *mode == wroom.ModeToken && len(*secret) < 8 {
		return fmt.Errorf("token mode needs -secret (RB_SECRET) of at least 8 characters")
	}
	releaseAt, err := epochMs(*release)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	room := wroom.New(wroom.Config{
		Policy: *policy, Mode: *mode, ReleaseAt: releaseAt, AdmitRate: *rate, Burst: *burst, MaxActive: *maxActive,
		SessionTTL: *session, AbandonAfter: *abandon, PollInterval: *poll, Secret: []byte(*secret), Seed: *seed, GatePath: *gate,
		LeaseRequests: *lease, Reentry: *reentry, Adaptive: *adaptive, TargetLatency: *target, SlowShare: *slowShare,
		ControlInterval: *ctlInterval, IncreaseStep: *incStep, DecreaseFactor: *decFactor, MinRate: *minRate, MaxRate: *maxRate,
	})
	h, err := wroom.NewHandler(room, *backend)
	if err != nil {
		return err
	}
	stopTick := make(chan struct{})
	defer close(stopTick)
	go room.Run(5*time.Millisecond, stopTick)
	log.Printf("waiting room policy=%s mode=%s admit_rate=%.1f/s lease=%d reentry=%s adaptive=%v target=%s backend=%s",
		*policy, *mode, *rate, *lease, *reentry, *adaptive, *target, *backend)
	return serve(ctx, newHTTPServer(*listen, h.Routes()))
}
