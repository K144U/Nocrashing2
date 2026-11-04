// Package oracle classifies every response the load generator receives by
// comparing it with the ground truth from package dataset.
package oracle

import (
	"bytes"
	"encoding/json"

	"github.com/k144u/nocrashing/resultbench/internal/dataset"
)

// Outcome is the class of one HTTP exchange.
type Outcome uint8

const (
	// Correct: the expected, current result for a valid roll number.
	Correct Outcome = iota
	// CorrectNotFound: a 404 for a roll number that does not exist (typo).
	CorrectNotFound
	// Stale: the old version of a result after a correction was published.
	Stale
	// Ahead: the corrected version before the correction was published
	// (should not happen; would indicate a clock or pipeline bug).
	Ahead
	// Wrong: a result that matches neither version, or another student's.
	Wrong
	// Missing: a 404 for a roll number that does exist.
	Missing
	// SoftFailure: HTTP 200 whose body is not a result (an error page that
	// claims success).
	SoftFailure
	// NotReleased: the portal says results are not out yet.
	NotReleased
	// Queued: a waiting room holding the user.
	Queued
	// Rejected: the server shed the request on purpose (429 or 503).
	Rejected
	// ServerError: any other 5xx.
	ServerError
	// ClientError: any other 4xx.
	ClientError
	// Timeout: no response before the user's patience ran out.
	Timeout
	// NetError: connection refused, reset, or similar.
	NetError
	numOutcomes
)

// NumOutcomes is the number of distinct outcomes.
const NumOutcomes = int(numOutcomes)

var names = [...]string{
	"correct", "correct_notfound", "stale", "ahead", "wrong", "missing", "soft_failure",
	"not_released", "queued", "rejected", "server_error", "client_error", "timeout", "net_error",
}

func (o Outcome) String() string { return names[o] }

// Final reports whether the outcome ends the user's search: the user saw a
// result (right or wrong) or was told the roll number does not exist.
func (o Outcome) Final() bool {
	switch o {
	case Correct, CorrectNotFound, Stale, Ahead, Wrong:
		return true
	}
	return false
}

// Failure reports whether the outcome is an availability failure that a
// user would typically answer by refreshing.
func (o Outcome) Failure() bool {
	switch o {
	case Missing, SoftFailure, Rejected, ServerError, ClientError, Timeout, NetError:
		return true
	}
	return false
}

// Status is the small JSON envelope used by gates and waiting rooms.
type Status struct {
	Status       string `json:"status"`
	Ticket       string `json:"ticket,omitempty"`
	Position     int64  `json:"position"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
	// EtaMs is the waiting room's estimate of the remaining wait; nil if
	// the room gives none, negative if it cannot estimate.
	EtaMs *int64 `json:"eta_ms,omitempty"`
}

// Oracle holds the dataset configuration.
type Oracle struct {
	Cfg dataset.Config
	// RollOnly checks only that a result belongs to the requested roll
	// number. Use it when load-testing a deployment with real data, for which
	// the synthetic ground truth does not apply; 404s then count as correct.
	RollOnly bool
}

// Classify decides the outcome of one response to GET /result?roll=roll.
// correctionsPublished says whether the board had published corrections when
// the request was sent.
func (o Oracle) Classify(code int, body []byte, roll int64, correctionsPublished bool) (Outcome, *Status) {
	idx, valid := o.Cfg.Valid(roll)
	if o.RollOnly {
		valid = false // unknown: a 404 may be right
	}
	switch {
	case code == 200:
		// fall through to body inspection below
	case code == 404:
		if valid {
			return Missing, nil
		}
		return CorrectNotFound, nil
	case code == 425:
		return NotReleased, nil
	case code == 429 || code == 503:
		return Rejected, nil
	case code >= 500:
		return ServerError, nil
	default:
		return ClientError, nil
	}

	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return SoftFailure, nil
	}
	// Gate and waiting-room envelopes carry a "status" field; results do not.
	if bytes.Contains(body, []byte(`"status"`)) {
		var st Status
		if json.Unmarshal(body, &st) == nil && st.Status != "" {
			switch st.Status {
			case "queued":
				return Queued, &st
			case "not_released":
				return NotReleased, &st
			}
		}
	}
	var rec dataset.Record
	if err := json.Unmarshal(body, &rec); err != nil || rec.Roll == 0 {
		return SoftFailure, nil
	}
	if o.RollOnly {
		if rec.Roll == roll {
			return Correct, nil
		}
		return Wrong, nil
	}
	if !valid || rec.Roll != roll {
		return Wrong, nil
	}
	got := dataset.Fingerprint(rec)
	want := o.Cfg.Current(idx, correctionsPublished)
	if got == dataset.Fingerprint(want) {
		return Correct, nil
	}
	if o.Cfg.IsCorrected(idx) {
		other := o.Cfg.Current(idx, !correctionsPublished)
		if got == dataset.Fingerprint(other) {
			if correctionsPublished {
				return Stale, nil
			}
			return Ahead, nil
		}
	}
	return Wrong, nil
}
