package watchdog

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "watchdog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}
func TestDurableQueueAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Telegram = true
	cfg.Email = true
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	states := map[string]AlertState{"cpu": {Level: "critical", Announced: "critical", LastQueued: now}}
	if err = s.CommitCycle(states, []PendingEvent{{"cpu", "critical", "message"}}, cfg, now); err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	loaded, err := s.States()
	if err != nil || loaded["cpu"].Level != "critical" {
		t.Fatal(loaded, err)
	}
	saved, err := s.Settings()
	if err != nil || !saved.Email {
		t.Fatal(saved, err)
	}
	for _, channel := range []string{"email", "telegram"} {
		d, err := s.Due(now)
		if err != nil || d == nil {
			t.Fatal("missing durable message", err)
		}
		_ = channel
		if err = s.Finish(*d, errors.New("temporary"), now); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := s.Due(now.Add(9 * time.Second)); err != nil || d != nil {
		t.Fatal("retry too soon", d, err)
	}
	for i := 0; i < 2; i++ {
		d, err := s.Due(now.Add(10 * time.Second))
		if err != nil || d == nil || d.Attempts != 1 {
			t.Fatal(d, err)
		}
		if err = s.Finish(*d, errors.New("temporary"), now.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		d, err := s.Due(now.Add(30 * time.Second))
		if err != nil || d == nil || d.Attempts != 2 {
			t.Fatal(d, err)
		}
		if err = s.Finish(*d, errors.New("final"), now.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := s.Due(now.Add(time.Hour)); err != nil || d != nil {
		t.Fatal("unbounded retries", d, err)
	}
	_, deliveries, err := s.History()
	if err != nil || len(deliveries) != 2 {
		t.Fatal(deliveries, err)
	}
	for _, d := range deliveries {
		if d.Status != "failed" || d.Attempts != 3 {
			t.Fatal(d)
		}
	}
}
func TestPauseCancelsQueueAndResumesActiveAlert(t *testing.T) {
	s := newTestStore(t)
	cfg := Defaults()
	cfg.Telegram = true
	if err := s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	states := map[string]AlertState{"cpu": {Level: "critical", Candidate: "critical", Announced: "critical", LastQueued: now}}
	if err := s.CommitCycle(states, []PendingEvent{{"cpu", "critical", "critical"}}, cfg, now); err != nil {
		t.Fatal(err)
	}
	cfg.PausedUntil = now.Add(time.Hour)
	if err := s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Due(now); err != nil || d != nil {
		t.Fatal("paused queue still active", d, err)
	}
	loaded, err := s.States()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded["cpu"].LastQueued.IsZero() {
		t.Fatal("resume marker not reset")
	}
	_, events := Evaluate(Snapshot{At: now.Add(time.Hour), Metrics: []Metric{{"cpu", "cpu", 99}}}, cfg, loaded)
	if len(events) != 1 {
		t.Fatal("no current alert on resume", events)
	}
}
func TestSupersedingAndDisabledRulesCancelQueue(t *testing.T) {
	s := newTestStore(t)
	cfg := Defaults()
	cfg.Telegram = true
	now := time.Now().UTC()
	states := map[string]AlertState{"cpu": {Level: "critical"}}
	if err := s.CommitCycle(states, []PendingEvent{{"cpu", "critical", "critical"}}, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitCycle(states, []PendingEvent{{"cpu", "ok", "recovery"}}, cfg, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	d, err := s.Due(now)
	if err != nil || d == nil || d.Message != "recovery" {
		t.Fatal(d, err)
	}
	r := cfg.Rules["cpu"]
	r.Enabled = false
	cfg.Rules["cpu"] = r
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if d, err = s.Due(now); err != nil || d != nil {
		t.Fatal("disabled rule message still pending", d, err)
	}
}
func TestCycleRollback(t *testing.T) {
	s := newTestStore(t)
	cfg := Defaults()
	cfg.Telegram = true
	now := time.Now().UTC()
	if _, err := s.DB.Exec("CREATE TRIGGER fail_delivery BEFORE INSERT ON deliveries BEGIN SELECT RAISE(ABORT, 'test'); END"); err != nil {
		t.Fatal(err)
	}
	err := s.CommitCycle(map[string]AlertState{"cpu": {Level: "critical"}}, []PendingEvent{{"cpu", "critical", "message"}}, cfg, now)
	if err == nil {
		t.Fatal("expected transaction failure")
	}
	states, err := s.States()
	if err != nil || len(states) != 0 {
		t.Fatal("state committed without message", states, err)
	}
	events, _, err := s.History()
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
}

func TestFailedInFlightDeliveryCannotResurrectCancelledQueue(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	if err := s.TestEvent("telegram", now); err != nil {
		t.Fatal(err)
	}
	d, err := s.Due(now)
	if err != nil || d == nil {
		t.Fatal(d, err)
	}
	if err = s.CancelPending("telegram"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(*d, errors.New("timeout"), now); err != nil {
		t.Fatal(err)
	}
	if due, err := s.Due(now.Add(time.Hour)); err != nil || due != nil {
		t.Fatal("cancelled delivery resurrected", due, err)
	}
}
