package watchdog

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }
type Event struct {
	ID      int64  `json:"id"`
	At      string `json:"at"`
	Metric  string `json:"metric"`
	Level   string `json:"level"`
	Message string `json:"message"`
}
type Delivery struct {
	ID       int64  `json:"id"`
	EventID  int64  `json:"event_id"`
	Channel  string `json:"channel"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	Next     int64  `json:"-"`
	Message  string `json:"-"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS config (id INTEGER PRIMARY KEY CHECK(id=1), body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS states (key TEXT PRIMARY KEY, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, at TEXT NOT NULL, metric TEXT NOT NULL, level TEXT NOT NULL, message TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS deliveries (id INTEGER PRIMARY KEY, event_id INTEGER NOT NULL REFERENCES events(id), channel TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', next INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS deliveries_due ON deliveries(status,next);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db}
	b, _ := json.Marshal(Defaults())
	_, err = db.Exec("INSERT OR IGNORE INTO config(id,body) VALUES(1,?)", string(b))
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Settings() (Settings, error) {
	var b string
	var cfg Settings
	err := s.DB.QueryRow("SELECT body FROM config WHERE id=1").Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &cfg)
	}
	return cfg, err
}
func (s *Store) SaveSettings(cfg Settings) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	old, err := s.Settings()
	if err != nil {
		return err
	}
	states, err := s.States()
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, _ := json.Marshal(cfg)
	if _, err = tx.Exec("UPDATE config SET body=? WHERE id=1", string(b)); err != nil {
		return err
	}
	muted := !cfg.Enabled || time.Now().Before(cfg.PausedUntil)
	for key, v := range states {
		kind := key
		mount := ""
		for i, c := range key {
			if c == ':' {
				kind = key[:i]
				mount = key[i+1:]
				break
			}
		}
		keep := cfg.Rules[kind].Enabled
		if mount != "" {
			found := false
			for _, p := range cfg.Mounts {
				if p == mount {
					found = true
				}
			}
			keep = keep && found
		}
		if !keep {
			if _, err = tx.Exec("UPDATE deliveries SET status='cancelled',error='Правило отключено или раздел удалён' WHERE status='pending' AND event_id IN (SELECT id FROM events WHERE metric=?)", key); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM states WHERE key=?", key); err != nil {
				return err
			}
			delete(states, key)
		} else if old.Rules[kind] != cfg.Rules[kind] {
			v.Candidate = ""
			v.Since = time.Time{}
			v.CriticalSince = time.Time{}
			states[key] = v
			b, _ = json.Marshal(v)
			if _, err = tx.Exec("UPDATE states SET body=? WHERE key=?", string(b), key); err != nil {
				return err
			}
		}
	}
	for _, ch := range []string{"telegram", "email"} {
		on := cfg.Telegram
		if ch == "email" {
			on = cfg.Email
		}
		if muted || !on {
			if _, err = tx.Exec("UPDATE deliveries SET status='cancelled',error='Уведомления отключены или приостановлены' WHERE status='pending' AND channel=?", ch); err != nil {
				return err
			}
		}
	}
	// Force a fresh active-state report when channels resume, including automatic pause expiry.
	if muted || old.Enabled != cfg.Enabled || old.Telegram != cfg.Telegram || old.Email != cfg.Email || !old.PausedUntil.Equal(cfg.PausedUntil) {
		for k, v := range states {
			v.LastQueued = time.Time{}
			b, _ = json.Marshal(v)
			if _, err = tx.Exec("UPDATE states SET body=? WHERE key=?", string(b), k); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (s *Store) States() (map[string]AlertState, error) {
	rows, err := s.DB.Query("SELECT key,body FROM states")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]AlertState{}
	for rows.Next() {
		var k, b string
		if err = rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		var v AlertState
		if err = json.Unmarshal([]byte(b), &v); err != nil {
			return nil, err
		}
		states[k] = v
	}
	return states, rows.Err()
}

type PendingEvent struct{ Metric, Level, Message string }

func (s *Store) CommitCycle(states map[string]AlertState, events []PendingEvent, cfg Settings, now time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM states"); err != nil {
		return err
	}
	for k, v := range states {
		b, _ := json.Marshal(v)
		if _, err = tx.Exec("INSERT INTO states(key,body) VALUES(?,?)", k, string(b)); err != nil {
			return err
		}
	}
	for _, e := range events {
		if _, err = tx.Exec("UPDATE deliveries SET status='cancelled',error='Заменено новым состоянием метрики' WHERE status='pending' AND event_id IN (SELECT id FROM events WHERE metric=?)", e.Metric); err != nil {
			return err
		}
		if err = insertEvent(tx, e, cfg, now); err != nil {
			return err
		}
	}
	// Retain 30 days of terminal history; pending work is never removed.
	_, err = tx.Exec(`DELETE FROM deliveries WHERE status != 'pending' AND event_id IN (SELECT id FROM events WHERE at < ?)`, now.Add(-30*24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM events WHERE at < ? AND id NOT IN (SELECT event_id FROM deliveries)`, now.Add(-30*24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}
func insertEvent(tx *sql.Tx, e PendingEvent, cfg Settings, now time.Time) error {
	r, err := tx.Exec("INSERT INTO events(at,metric,level,message) VALUES(?,?,?,?)", now.Format(time.RFC3339Nano), e.Metric, e.Level, e.Message)
	if err != nil {
		return err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return err
	}
	for channel, on := range map[string]bool{"telegram": cfg.Telegram, "email": cfg.Email} {
		if on {
			if _, err = tx.Exec("INSERT INTO deliveries(event_id,channel) VALUES(?,?)", id, channel); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) TestEvent(channel string, now time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = insertEvent(tx, PendingEvent{"test", "test", "Watchdog: тестовое уведомление"}, Settings{Telegram: channel == "telegram", Email: channel == "email"}, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) History() ([]Event, []Delivery, error) {
	events := []Event{}
	deliveries := []Delivery{}
	rows, err := s.DB.Query("SELECT id,at,metric,level,message FROM events ORDER BY id DESC LIMIT 100")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.At, &e.Metric, &e.Level, &e.Message); err != nil {
			rows.Close()
			return nil, nil, err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.DB.Query("SELECT id,event_id,channel,status,attempts,error FROM deliveries ORDER BY id DESC LIMIT 200")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.ID, &d.EventID, &d.Channel, &d.Status, &d.Attempts, &d.Error); err != nil {
			return nil, nil, err
		}
		deliveries = append(deliveries, d)
	}
	return events, deliveries, rows.Err()
}
func (s *Store) Due(now time.Time) (*Delivery, error) {
	var d Delivery
	err := s.DB.QueryRow(`SELECT d.id,d.event_id,d.channel,d.status,d.attempts,d.error,d.next,e.message FROM deliveries d JOIN events e ON e.id=d.event_id WHERE d.status='pending' AND d.next<=? ORDER BY d.id LIMIT 1`, now.Unix()).Scan(&d.ID, &d.EventID, &d.Channel, &d.Status, &d.Attempts, &d.Error, &d.Next, &d.Message)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &d, err
}
func (s *Store) Finish(d Delivery, sendErr error, now time.Time) error {
	status, detail, next := "sent", "", int64(0)
	if sendErr != nil {
		detail = sendErr.Error()
		status = "pending"
		next = now.Add(time.Duration(1<<d.Attempts) * 10 * time.Second).Unix()
		if d.Attempts >= 2 {
			status = "failed"
		}
		_, err := s.DB.Exec("UPDATE deliveries SET attempts=attempts+1 WHERE id=? AND status='cancelled'", d.ID)
		if err != nil {
			return err
		}
		_, err = s.DB.Exec("UPDATE deliveries SET status=?,attempts=attempts+1,error=?,next=? WHERE id=? AND status='pending'", status, detail, next, d.ID)
		return err
	}
	_, err := s.DB.Exec("UPDATE deliveries SET status=?,attempts=attempts+1,error=?,next=? WHERE id=?", status, detail, next, d.ID)
	return err
}
func (s *Store) CancelPending(channel string) error {
	_, err := s.DB.Exec("UPDATE deliveries SET status='cancelled',error='Уведомления отключены или приостановлены' WHERE status='pending' AND channel=?", channel)
	return err
}
