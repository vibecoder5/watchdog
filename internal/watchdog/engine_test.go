package watchdog

import (
	"testing"
	"time"
)

func TestAlertLifecycle(t *testing.T) {
	cfg := Defaults()
	cfg.Telegram = true
	states := map[string]AlertState{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	step := func(second int, value float64, want string, wantEvents int) {
		t.Helper()
		var events []PendingEvent
		states, events = Evaluate(Snapshot{At: base.Add(time.Duration(second) * time.Second), Host: "test", Metrics: []Metric{{"cpu", "cpu", value}}}, cfg, states)
		if states["cpu"].Level != want || len(events) != wantEvents {
			t.Fatalf("at %d: state=%+v events=%+v; want %s / %d", second, states["cpu"], events, want, wantEvents)
		}
	}
	step(0, 90, "ok", 0)
	step(15, 90, "ok", 0)
	step(30, 90, "ok", 0)
	step(45, 90, "ok", 0)
	step(60, 90, "warning", 1)
	step(75, 84, "warning", 0)
	step(90, 90, "warning", 0)
	step(105, 97, "warning", 0)
	step(120, 97, "warning", 0)
	step(135, 97, "warning", 0)
	step(150, 97, "warning", 0)
	step(165, 97, "critical", 1)
	step(180, 92, "critical", 0)
	step(195, 79, "ok", 1)
	step(210, 50, "ok", 0)
}
func TestPauseAndMissingSample(t *testing.T) {
	cfg := Defaults()
	cfg.Telegram = true
	base := time.Now().UTC()
	cfg.PausedUntil = base.Add(time.Minute)
	r := cfg.Rules["cpu"]
	r.HoldSeconds = 0
	cfg.Rules["cpu"] = r
	snapshot := Snapshot{At: base, Metrics: []Metric{{"cpu", "cpu", 99}}}
	states, events := Evaluate(snapshot, cfg, nil)
	if len(events) != 0 || states["cpu"].Level != "critical" {
		t.Fatal(states, events)
	}
	snapshot.At = base.Add(time.Minute)
	states, events = Evaluate(snapshot, cfg, states)
	if len(events) != 1 {
		t.Fatal("pause did not expire", events)
	}
	snapshot.At = snapshot.At.Add(15 * time.Second)
	_, events = Evaluate(snapshot, cfg, states)
	if len(events) != 0 {
		t.Fatal("duplicate notification")
	}
	cfg.PausedUntil = time.Time{}
	r.HoldSeconds = 60
	cfg.Rules["cpu"] = r
	states = nil
	snapshot.At = base
	states, _ = Evaluate(snapshot, cfg, states)
	states, _ = Evaluate(Snapshot{At: base.Add(15 * time.Second)}, cfg, states)
	snapshot.At = base.Add(60 * time.Second)
	states, events = Evaluate(snapshot, cfg, states)
	if states["cpu"].Level != "ok" || len(events) != 0 {
		t.Fatal("missing samples counted toward hold")
	}
}
func TestRepeatAndRestartGap(t *testing.T) {
	cfg := Defaults()
	cfg.Telegram = true
	r := cfg.Rules["ram"]
	r.HoldSeconds = 0
	cfg.Rules["ram"] = r
	base := time.Now().UTC()
	snapshot := Snapshot{At: base, Metrics: []Metric{{"ram", "ram", 90}}}
	states, events := Evaluate(snapshot, cfg, nil)
	if len(events) != 1 {
		t.Fatal(events)
	}
	snapshot.At = base.Add(59 * time.Minute)
	states, events = Evaluate(snapshot, cfg, states)
	if len(events) != 0 {
		t.Fatal("early reminder")
	}
	snapshot.At = base.Add(time.Hour)
	_, events = Evaluate(snapshot, cfg, states)
	if len(events) != 1 {
		t.Fatal("missing reminder")
	}
	r.HoldSeconds = 60
	cfg.Rules["ram"] = r
	snapshot.At = base
	states, _ = Evaluate(snapshot, cfg, nil)
	snapshot.At = base.Add(10 * time.Minute)
	states, events = Evaluate(snapshot, cfg, states)
	if len(events) != 0 || states["ram"].Level != "ok" {
		t.Fatal("downtime counted toward hold")
	}
}
func TestSettingsValidation(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Settings){func(s *Settings) { s.Interval = 0 }, func(s *Settings) { s.Mounts = []string{"/", "/"} }, func(s *Settings) { s.Mounts = []string{"relative"} }, func(s *Settings) { r := s.Rules["cpu"]; r.Critical = r.Warning; s.Rules["cpu"] = r }, func(s *Settings) { s.Rules["extra"] = Rule{} }} {
		cfg = Defaults()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("invalid settings accepted", cfg)
		}
	}
}

func TestOscillationAtCriticalStillWarns(t *testing.T) {
	cfg := Defaults()
	cfg.Telegram = true
	base := time.Now().UTC()
	states := map[string]AlertState{}
	for i, value := range []float64{94, 96, 94, 96, 94} {
		var events []PendingEvent
		states, events = Evaluate(Snapshot{At: base.Add(time.Duration(i*15) * time.Second), Metrics: []Metric{{"cpu", "cpu", value}}}, cfg, states)
		if i == 4 && (states["cpu"].Level != "warning" || len(events) != 1) {
			t.Fatal("critical oscillation hid sustained warning", states, events)
		}
	}
}
