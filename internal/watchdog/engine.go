package watchdog

import (
	"fmt"
	"time"
)

func level(value float64, r Rule, previous string) string {
	if value >= r.Critical || (previous == "critical" && value > r.Critical-r.Hysteresis) {
		return "critical"
	}
	if value >= r.Warning || (previous != "" && previous != "ok" && value > r.Warning-r.Hysteresis) {
		return "warning"
	}
	return "ok"
}

// Evaluate is deterministic; callers persist both states and outbox in one transaction.
func Evaluate(snapshot Snapshot, cfg Settings, old map[string]AlertState) (map[string]AlertState, []PendingEvent) {
	states := map[string]AlertState{}
	for k, v := range old {
		states[k] = v
	}
	events := []PendingEvent{}
	now := snapshot.At
	active := cfg.Enabled && !now.Before(cfg.PausedUntil) && (cfg.Telegram || cfg.Email)
	seen := map[string]bool{}
	for _, m := range snapshot.Metrics {
		seen[m.Key] = true
		r, ok := cfg.Rules[m.Kind]
		if !ok || !r.Enabled {
			delete(states, m.Key)
			continue
		}
		s := states[m.Key]
		if !s.SeenAt.IsZero() && now.Sub(s.SeenAt) > time.Duration(cfg.Interval*2)*time.Second {
			s.Candidate = ""
			s.Since = time.Time{}
			s.CriticalSince = time.Time{}
		}
		s.SeenAt = now
		if s.Level == "" {
			s.Level = "ok"
		}
		target := level(m.Value, r, s.Level)
		if target == "ok" {
			s.Level = "ok"
			s.Since = time.Time{}
			s.CriticalSince = time.Time{}
		} else {
			if s.Candidate == "" || s.Candidate == "ok" || s.Since.IsZero() {
				s.Since = now
			}
			if target == "critical" {
				if s.Candidate != "critical" || s.CriticalSince.IsZero() {
					s.CriticalSince = now
				}
			} else {
				s.CriticalSince = time.Time{}
			}
			hold := time.Duration(r.HoldSeconds) * time.Second
			if target == "critical" && now.Sub(s.CriticalSince) >= hold {
				s.Level = "critical"
			} else if target == "warning" && s.Level == "critical" {
				s.Level = "warning"
			} else if s.Level == "ok" && now.Sub(s.Since) >= hold {
				s.Level = "warning"
			}
		}
		s.Candidate = target
		if active {
			should := s.Level != "ok" && (s.Announced != s.Level || s.LastQueued.IsZero() || now.Sub(s.LastQueued) >= time.Duration(cfg.RepeatMinutes)*time.Minute)
			recovered := s.Level == "ok" && s.Announced != "" && s.Announced != "ok"
			if should || recovered {
				message := fmt.Sprintf("Watchdog · %s\n%s: %.1f%% — %s\n%s", snapshot.Host, m.Key, m.Value, s.Level, now.Format(time.RFC3339))
				events = append(events, PendingEvent{m.Key, s.Level, message})
				s.LastQueued = now
				s.Announced = s.Level
			}
		}
		states[m.Key] = s
	}
	// Missing samples must not count toward a continuous threshold breach.
	for key, s := range states {
		if !seen[key] {
			s.Candidate = ""
			s.Since = time.Time{}
			s.CriticalSince = time.Time{}
			states[key] = s
		}
	}
	for key := range states {
		kind := key
		for i, c := range key {
			if c == ':' {
				kind = key[:i]
				break
			}
		}
		r, ok := cfg.Rules[kind]
		if !ok || !r.Enabled {
			delete(states, key)
			continue
		}
		if kind == "disk" || kind == "inode" {
			found := false
			for _, p := range cfg.Mounts {
				if key == kind+":"+p {
					found = true
				}
			}
			if !found {
				delete(states, key)
			}
		}
	}
	return states, events
}
