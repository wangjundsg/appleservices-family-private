package pacing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDurationIsDeterministicAndBounded(t *testing.T) {
	cfg := Config{Enabled: true, Min: 100 * time.Millisecond, Max: 900 * time.Millisecond, Seed: 42}
	a := New(cfg, func(context.Context, time.Duration) error { return nil })
	b := New(cfg, func(context.Context, time.Duration) error { return nil })
	for _, edge := range [][2]string{{"start", "preflight"}, {"preflight", "captcha"}, {"captcha", "validate"}} {
		d1 := a.Duration(edge[0], edge[1])
		d2 := b.Duration(edge[0], edge[1])
		if d1 != d2 {
			t.Fatalf("%v: same seed produced %v and %v", edge, d1, d2)
		}
		if d1 < cfg.Min || d1 >= cfg.Max {
			t.Fatalf("%v: %v outside [%v,%v)", edge, d1, cfg.Min, cfg.Max)
		}
	}
}

func TestDifferentEdgesGetDifferentDelays(t *testing.T) {
	cfg := Config{Enabled: true, Min: 0, Max: 10 * time.Second, Seed: 7}
	p := New(cfg, func(context.Context, time.Duration) error { return nil })
	seen := map[time.Duration]bool{}
	for _, edge := range [][2]string{{"a", "b"}, {"b", "c"}, {"c", "d"}, {"d", "e"}} {
		seen[p.Duration(edge[0], edge[1])] = true
	}
	if len(seen) < 3 {
		t.Fatalf("edges collapsed onto %d distinct delays: %v", len(seen), seen)
	}
}

func TestDisabledPacerWaitsNothing(t *testing.T) {
	p := New(Config{Enabled: false, Min: time.Second, Max: 2 * time.Second}, func(context.Context, time.Duration) error {
		t.Fatal("disabled pacer must not sleep")
		return nil
	})
	if d := p.Duration("a", "b"); d != 0 {
		t.Fatalf("disabled duration = %v", d)
	}
	if _, err := p.Before(context.Background(), "a", "b"); err != nil {
		t.Fatalf("Before: %v", err)
	}
	if p.Enabled() {
		t.Fatal("Enabled should be false")
	}
}

func TestBeforeRecordsAndRespectsCancellation(t *testing.T) {
	var waited []time.Duration
	cfg := Config{Enabled: true, Min: 50 * time.Millisecond, Max: 60 * time.Millisecond, Seed: 3}
	p := New(cfg, func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	})
	if _, err := p.Before(context.Background(), "start", "preflight"); err != nil {
		t.Fatalf("Before: %v", err)
	}
	if len(waited) != 1 {
		t.Fatalf("expected one sleep, got %v", waited)
	}
	if got := p.Applied(); len(got) != 1 {
		t.Fatalf("Applied = %v", got)
	}

	boom := errors.New("cancelled")
	cancelled := New(cfg, func(context.Context, time.Duration) error { return boom })
	if _, err := cancelled.Before(context.Background(), "a", "b"); !errors.Is(err, boom) {
		t.Fatalf("cancellation must surface, got %v", err)
	}
}
