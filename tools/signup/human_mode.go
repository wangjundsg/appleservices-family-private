package main

// Human mode: the pacing configuration and the warm-up receipt, frozen with the
// task so a resume keeps the same rhythm.

import (
	"hash/fnv"
	"time"

	"github.com/Laky-64/appleservices/internal/pacing"
)

// humanConfig is stored in the task state. Changing it after the first request
// would change the rhythm of a session Apple already saw, so it is frozen.
type humanConfig struct {
	Pacing pacing.Config `json:"pacing"`
	Assets int           `json:"assets"`
}

// freezeHuman returns the task's human settings: the stored ones when the task
// already has them, otherwise a fresh configuration derived from the task
// identity (so the seed is stable for this task and not derived from the clock).
func freezeHuman(st *state, enabled bool, min, max time.Duration, assets int, session *Session) (*humanConfig, error) {
	if st.Human != nil {
		return st.Human, nil
	}
	if !enabled {
		// A resumed legacy task can still carry pacing if a previous run set it;
		// st.Human == nil here means it never did.
		return nil, nil
	}
	if min < 0 || max < 0 || max < min {
		return nil, errHumanBounds
	}
	if assets < 0 || assets > 8 {
		return nil, errHumanAssets
	}
	seed := seedFromTask(st, session)
	cfg := humanConfig{Pacing: pacing.Config{Enabled: true, Min: min, Max: max, Seed: seed}, Assets: assets}
	st.Human = &cfg
	return &cfg, nil
}

func seedFromTask(st *state, session *Session) int64 {
	h := fnv.New64a()
	identity := st.Email
	if st.Profile != nil {
		identity += "|" + st.Profile.ID
	}
	if session != nil {
		identity += "|" + session.locale + "|" + session.timezone
	}
	_, _ = h.Write([]byte(identity))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

var (
	errHumanBounds = errString("pacing bounds are invalid")
	errHumanAssets = errString("warm-up asset count must be between 0 and 8")
)

type errString string

func (e errString) Error() string { return string(e) }
