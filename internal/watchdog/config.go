package watchdog

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

type Rule struct {
	Enabled     bool    `json:"enabled"`
	Warning     float64 `json:"warning"`
	Critical    float64 `json:"critical"`
	HoldSeconds int     `json:"hold_seconds"`
	Hysteresis  float64 `json:"hysteresis"`
}

type Settings struct {
	Interval      int             `json:"interval"`
	RepeatMinutes int             `json:"repeat_minutes"`
	Enabled       bool            `json:"enabled"`
	PausedUntil   time.Time       `json:"paused_until"`
	Mounts        []string        `json:"mounts"`
	Rules         map[string]Rule `json:"rules"`
	Telegram      bool            `json:"telegram"`
	Email         bool            `json:"email"`
}

func Defaults() Settings {
	return Settings{Interval: 15, RepeatMinutes: 60, Enabled: true, Mounts: []string{"/"}, Rules: map[string]Rule{
		"cpu": {true, 85, 95, 60, 5}, "ram": {true, 85, 95, 60, 5},
		"swap": {true, 80, 95, 60, 5}, "disk": {true, 80, 90, 30, 3}, "inode": {true, 80, 90, 30, 3},
	}}
}

func (s Settings) Validate() error {
	if s.Interval < 5 || s.Interval > 300 {
		return errors.New("интервал должен быть от 5 до 300 секунд")
	}
	if s.RepeatMinutes < 5 || s.RepeatMinutes > 1440 {
		return errors.New("повтор должен быть от 5 до 1440 минут")
	}
	if len(s.Mounts) == 0 || len(s.Mounts) > 32 {
		return errors.New("укажите от 1 до 32 точек монтирования")
	}
	seen := map[string]bool{}
	for _, p := range s.Mounts {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || seen[p] || strings.ContainsAny(p, "\n\r\x00") {
			return errors.New("некорректная или повторяющаяся точка монтирования")
		}
		seen[p] = true
	}
	if len(s.Rules) != 5 {
		return errors.New("требуется ровно пять правил")
	}
	for _, k := range []string{"cpu", "ram", "swap", "disk", "inode"} {
		r, ok := s.Rules[k]
		if !ok || r.Warning <= 0 || r.Critical <= r.Warning || r.Critical > 100 || r.Hysteresis <= 0 || r.Hysteresis >= r.Warning || r.HoldSeconds < 0 || r.HoldSeconds > 3600 {
			return fmt.Errorf("некорректное правило %s", k)
		}
	}
	return nil
}

type Metric struct {
	Key   string  `json:"key"`
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
}
type Snapshot struct {
	At      time.Time `json:"at"`
	Host    string    `json:"host"`
	Metrics []Metric  `json:"metrics"`
	Errors  []string  `json:"errors"`
}
type AlertState struct {
	CriticalSince time.Time `json:"critical_since"`
	SeenAt        time.Time `json:"seen_at"`
	Level         string    `json:"level"`
	Candidate     string    `json:"candidate"`
	Since         time.Time `json:"since"`
	LastQueued    time.Time `json:"last_queued"`
	Announced     string    `json:"announced"`
}
