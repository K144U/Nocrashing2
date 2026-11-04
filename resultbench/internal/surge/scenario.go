// Package surge is the ResultBench load generator. It creates an open-loop
// stream of students arriving around release time, models how each student
// refreshes and retries, checks every response with the oracle, and records
// per-request, per-user and per-second results.
package surge

import (
	"fmt"
	"math"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/k144u/nocrashing/resultbench/internal/dataset"
)

// Range is a [min, max] pair sampled uniformly.
type Range [2]float64

// Scenario is the full description of one run. It is read from YAML; the
// runner overrides fields such as targets, release time and peak rate.
type Scenario struct {
	Name    string   `yaml:"name" json:"name"`
	Targets []string `yaml:"targets" json:"targets"`
	// MirrorSwitchProb is the chance that a student tries a different mirror
	// link after a failure (only with more than one target).
	MirrorSwitchProb float64 `yaml:"mirror_switch_prob" json:"mirror_switch_prob"`

	// ReleaseAtUnixMs fixes the release moment shared with the system under
	// test. If zero, release happens ReleaseInS seconds after the run starts.
	ReleaseAtUnixMs int64   `yaml:"release_at_unix_ms" json:"release_at_unix_ms"`
	ReleaseInS      float64 `yaml:"release_in_s" json:"release_in_s"`

	Dataset     DatasetSpec     `yaml:"dataset" json:"dataset"`
	Surge       SurgeSpec       `yaml:"surge" json:"surge"`
	Keys        KeySpec         `yaml:"keys" json:"keys"`
	Users       UserSpec        `yaml:"users" json:"users"`
	Corrections CorrectionSpec  `yaml:"corrections" json:"corrections"`
	Generator   GeneratorSpec   `yaml:"generator" json:"generator"`
	DeadlinesS  []float64       `yaml:"deadlines_s" json:"deadlines_s"`
	WaitingRoom WaitingRoomSpec `yaml:"waiting_room" json:"waiting_room"`
	Bots        BotSpec         `yaml:"bots" json:"bots"`
	// Oracle is "dataset" (check every field against the synthetic ground
	// truth) or "roll_only" (check only the roll number; for real data).
	Oracle string `yaml:"oracle" json:"oracle"`
}

// DatasetSpec must match the dataset loaded into the system under test.
type DatasetSpec struct {
	Seed               uint64 `yaml:"seed" json:"seed"`
	N                  int    `yaml:"n" json:"n"`
	RollBase           int64  `yaml:"roll_base" json:"roll_base"`
	CorrectionPerMille int    `yaml:"correction_per_mille" json:"correction_per_mille"`
	NumSchools         int    `yaml:"num_schools" json:"num_schools"`
}

// Config converts the spec to a dataset configuration.
func (d DatasetSpec) Config() dataset.Config {
	return dataset.Config{Seed: d.Seed, N: d.N, RollBase: d.RollBase, CorrectionPerMille: d.CorrectionPerMille, NumSchools: d.NumSchools}
}

// SurgeSpec is the arrival curve of new students (users per second).
//
//	shape "surge":    ramp before release, jump to the peak over rise_s, then
//	                  exponential decay with time constant decay_tau_s towards
//	                  floor_frac * peak.
//	shape "constant": peak_users_per_s from release until post_release_s
//	                  (used for calibration).
type SurgeSpec struct {
	Shape               string  `yaml:"shape" json:"shape"`
	PeakUsersPerS       float64 `yaml:"peak_users_per_s" json:"peak_users_per_s"`
	PreReleaseS         float64 `yaml:"pre_release_s" json:"pre_release_s"`
	PreReleaseStartFrac float64 `yaml:"pre_release_start_frac" json:"pre_release_start_frac"`
	PreReleaseEndFrac   float64 `yaml:"pre_release_end_frac" json:"pre_release_end_frac"`
	RiseS               float64 `yaml:"rise_s" json:"rise_s"`
	DecayTauS           float64 `yaml:"decay_tau_s" json:"decay_tau_s"`
	FloorFrac           float64 `yaml:"floor_frac" json:"floor_frac"`
	PostReleaseS        float64 `yaml:"post_release_s" json:"post_release_s"`
	// Arrivals is "poisson" (independent students, the default) or "even"
	// (evenly spaced, as k6's arrival-rate executors do; for cross-checks).
	Arrivals string `yaml:"arrivals" json:"arrivals"`
}

// KeySpec decides which roll number each student looks up.
//
//	uniform: every arriving student is a different student (sampling without
//	         replacement through a random permutation), so cold misses are
//	         on distinct keys.
//	zipf:    roll numbers follow a Zipf law (toppers, popular schools, family
//	         members re-checking), so some keys are hot.
type KeySpec struct {
	Dist     string  `yaml:"dist" json:"dist"`
	ZipfS    float64 `yaml:"zipf_s" json:"zipf_s"`
	TypoRate float64 `yaml:"typo_rate" json:"typo_rate"`
}

// UserSpec is the human behaviour model. Every parameter is an assumption
// (no study has measured refresh behaviour on result day) and should be
// varied in sensitivity runs.
type UserSpec struct {
	// PatienceS: how long a student waits for a response before giving up on
	// it and refreshing. Sampled once per student.
	PatienceS Range `yaml:"patience_s" json:"patience_s"`
	// RetryProb: chance to try again after an error page or empty response.
	RetryProb float64 `yaml:"retry_prob" json:"retry_prob"`
	// ImpatientRetryProb: chance to refresh after patience ran out.
	ImpatientRetryProb float64 `yaml:"impatient_retry_prob" json:"impatient_retry_prob"`
	RetryDelayS        Range   `yaml:"retry_delay_s" json:"retry_delay_s"`
	MaxAttempts        int     `yaml:"max_attempts" json:"max_attempts"`
	// PreReleasePollS: how often a student who arrived early reloads the page
	// while waiting for release.
	PreReleasePollS Range `yaml:"prerelease_poll_s" json:"prerelease_poll_s"`
	// QueueManualRefreshPerMin: how often a student in a waiting room presses
	// refresh, on top of the page's own polling.
	QueueManualRefreshPerMin float64 `yaml:"queue_manual_refresh_per_min" json:"queue_manual_refresh_per_min"`
	// RepeatViewProb: chance a student who got the result looks again later
	// (to show family, take a screenshot).
	RepeatViewProb float64 `yaml:"repeat_view_prob" json:"repeat_view_prob"`
	RepeatAfterS   Range   `yaml:"repeat_after_s" json:"repeat_after_s"`
	MaxSessionS    float64 `yaml:"max_session_s" json:"max_session_s"`
	// QueuePatienceS: how long a student stays in a waiting room's queue
	// (counted from release, or from joining if later) before giving up.
	// Sampled once per student. [0, 0] means no limit other than
	// MaxSessionS, as in the first paper.
	QueuePatienceS Range `yaml:"queue_patience_s" json:"queue_patience_s"`
	// EtaBalk: if positive and the room shows a wait estimate, a queued
	// student leaves as soon as time already waited plus the estimate exceeds
	// EtaBalk times their queue patience. 0 ignores estimates.
	EtaBalk float64 `yaml:"eta_balk" json:"eta_balk"`
}

// BotSpec describes automated clients (result scrapers, as run by
// aggregator sites) that walk through roll numbers and never give up. They
// are recorded but kept out of every student metric.
type BotSpec struct {
	Count int `yaml:"count" json:"count"`
	// StartS is when the first bot starts, relative to release; bots start
	// evenly over SpreadS seconds.
	StartS  float64 `yaml:"start_s" json:"start_s"`
	SpreadS float64 `yaml:"spread_s" json:"spread_s"`
	// RollsEach: consecutive roll numbers each bot fetches.
	RollsEach int `yaml:"rolls_each" json:"rolls_each"`
	// PollS: pause after a queued, not-released or failed answer (bots
	// ignore the room's suggested retry time). GapS: pause between rolls.
	PollS float64 `yaml:"poll_s" json:"poll_s"`
	GapS  float64 `yaml:"gap_s" json:"gap_s"`
	// TimeoutS: how long a bot waits for one response.
	TimeoutS float64 `yaml:"timeout_s" json:"timeout_s"`
	// KeepCookie: the bot keeps the waiting room's cookie across requests
	// (one session for all its rolls) instead of starting fresh each time.
	KeepCookie bool `yaml:"keep_cookie" json:"keep_cookie"`
}

// CorrectionSpec tells the oracle when corrections are published.
type CorrectionSpec struct {
	Enabled bool    `yaml:"enabled" json:"enabled"`
	AtS     float64 `yaml:"at_s" json:"at_s"` // seconds after release
}

// GeneratorSpec bounds the generator itself.
type GeneratorSpec struct {
	MaxUsers int     `yaml:"max_users" json:"max_users"`
	Seed     uint64  `yaml:"seed" json:"seed"`
	DrainS   float64 `yaml:"drain_s" json:"drain_s"`
	// MaxBodyBytes caps how much of each response is read.
	MaxBodyBytes int `yaml:"max_body_bytes" json:"max_body_bytes"`
}

// WaitingRoomSpec tells the client how the waiting room carries tickets.
type WaitingRoomSpec struct {
	// Mode is "naive" (ticket in page only, lost on manual refresh) or
	// "token" (signed cookie, kept). Ignored when there is no waiting room.
	Mode string `yaml:"mode" json:"mode"`
}

// Default returns a scenario with the documented default assumptions.
func Default() Scenario {
	d := dataset.Default()
	return Scenario{
		Name:             "default",
		Targets:          []string{"http://localhost:8080"},
		MirrorSwitchProb: 0.5,
		ReleaseInS:       30,
		Dataset:          DatasetSpec{Seed: d.Seed, N: d.N, RollBase: d.RollBase, CorrectionPerMille: d.CorrectionPerMille, NumSchools: d.NumSchools},
		Surge: SurgeSpec{
			Shape: "surge", PeakUsersPerS: 100, PreReleaseS: 30, PreReleaseStartFrac: 0.02, PreReleaseEndFrac: 0.2,
			RiseS: 5, DecayTauS: 120, FloorFrac: 0.02, PostReleaseS: 300, Arrivals: "poisson",
		},
		Keys: KeySpec{Dist: "uniform", ZipfS: 1.1, TypoRate: 0.01},
		Users: UserSpec{
			PatienceS: Range{5, 20}, RetryProb: 0.8, ImpatientRetryProb: 0.9, RetryDelayS: Range{1, 5},
			MaxAttempts: 20, PreReleasePollS: Range{5, 30}, QueueManualRefreshPerMin: 0.5,
			RepeatViewProb: 0.1, RepeatAfterS: Range{30, 120}, MaxSessionS: 1800,
		},
		Corrections: CorrectionSpec{AtS: 120},
		Generator:   GeneratorSpec{MaxUsers: 200_000, Seed: 1, DrainS: 60, MaxBodyBytes: 64 << 10},
		DeadlinesS:  []float64{2, 5, 10},
		WaitingRoom: WaitingRoomSpec{Mode: "token"},
		Bots:        BotSpec{RollsEach: 1000, PollS: 1, TimeoutS: 10, KeepCookie: true},
		Oracle:      "dataset",
	}
}

// Load reads a YAML scenario on top of the defaults.
func Load(path string) (Scenario, error) {
	sc := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	if err := yaml.Unmarshal(b, &sc); err != nil {
		return sc, fmt.Errorf("%s: %w", path, err)
	}
	return sc, sc.Validate()
}

// Validate checks the scenario for mistakes that would silently produce
// meaningless results.
func (s *Scenario) Validate() error {
	switch {
	case len(s.Targets) == 0:
		return fmt.Errorf("no targets")
	case s.Dataset.N <= 0:
		return fmt.Errorf("dataset.n must be positive")
	case s.Surge.PeakUsersPerS <= 0:
		return fmt.Errorf("surge.peak_users_per_s must be positive")
	case s.Surge.Shape != "surge" && s.Surge.Shape != "constant":
		return fmt.Errorf("surge.shape must be surge or constant")
	case s.Surge.Arrivals != "poisson" && s.Surge.Arrivals != "even":
		return fmt.Errorf("surge.arrivals must be poisson or even")
	case s.Keys.Dist != "uniform" && s.Keys.Dist != "zipf":
		return fmt.Errorf("keys.dist must be uniform or zipf")
	case s.Keys.Dist == "zipf" && s.Keys.ZipfS <= 1:
		return fmt.Errorf("keys.zipf_s must be > 1")
	case s.Users.PatienceS[0] <= 0 || s.Users.PatienceS[1] < s.Users.PatienceS[0]:
		return fmt.Errorf("users.patience_s must be a positive range")
	case s.Users.MaxAttempts <= 0:
		return fmt.Errorf("users.max_attempts must be positive")
	case s.Oracle != "dataset" && s.Oracle != "roll_only":
		return fmt.Errorf("oracle must be dataset or roll_only")
	case s.WaitingRoom.Mode != "naive" && s.WaitingRoom.Mode != "token":
		return fmt.Errorf("waiting_room.mode must be naive or token")
	case s.Users.QueuePatienceS[0] < 0 || s.Users.QueuePatienceS[1] < s.Users.QueuePatienceS[0]:
		return fmt.Errorf("users.queue_patience_s must be a range with min <= max")
	case s.Users.EtaBalk < 0:
		return fmt.Errorf("users.eta_balk must not be negative")
	case s.Bots.Count < 0:
		return fmt.Errorf("bots.count must not be negative")
	case s.Bots.Count > 0 && (s.Bots.RollsEach <= 0 || s.Bots.PollS <= 0 || s.Bots.TimeoutS <= 0):
		return fmt.Errorf("bots need rolls_each, poll_s and timeout_s > 0")
	}
	return nil
}

// Rate is the arrival rate of new students (per second) at t seconds
// relative to release.
func (s SurgeSpec) Rate(t float64) float64 {
	peak := s.PeakUsersPerS
	if t > s.PostReleaseS {
		return 0
	}
	if s.Shape == "constant" {
		if t < 0 {
			return 0
		}
		return peak
	}
	switch {
	case t < -s.PreReleaseS:
		return 0
	case t < 0:
		f := (t + s.PreReleaseS) / s.PreReleaseS
		return peak * (s.PreReleaseStartFrac + (s.PreReleaseEndFrac-s.PreReleaseStartFrac)*f)
	case t < s.RiseS:
		f := t / s.RiseS
		return peak * (s.PreReleaseEndFrac + (1-s.PreReleaseEndFrac)*f)
	default:
		floor := s.FloorFrac * peak
		tau := math.Max(s.DecayTauS, 1e-9)
		return floor + (peak-floor)*math.Exp(-(t-s.RiseS)/tau)
	}
}

// Start is the first moment (relative to release) with arrivals.
func (s SurgeSpec) Start() float64 {
	if s.Shape == "constant" {
		return 0
	}
	return -s.PreReleaseS
}

// ExpectedUsers integrates the arrival curve numerically.
func (s SurgeSpec) ExpectedUsers() float64 {
	total := 0.0
	const dt = 0.1
	for t := s.Start(); t <= s.PostReleaseS; t += dt {
		total += s.Rate(t) * dt
	}
	return total
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
