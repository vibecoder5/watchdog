package watchdog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web/*
var assets embed.FS

type session struct{ Expires time.Time }
type loginAttempts struct {
	Count int
	Until time.Time
}
type App struct {
	Store        *Store
	Notifier     *Notifier
	PasswordHash []byte
	SecureCookie bool
	mu           sync.Mutex
	snapshot     Snapshot
	cycleError   string
	lastCycle    time.Time
	sessions     map[[32]byte]session
	attempts     map[string]loginAttempts
}

func NewApp(store *Store, n *Notifier, hash []byte, secure bool) *App {
	return &App{Store: store, Notifier: n, PasswordHash: hash, SecureCookie: secure, sessions: map[[32]byte]session{}, attempts: map[string]loginAttempts{}, snapshot: Snapshot{Metrics: []Metric{}, Errors: []string{}}}
}

func (a *App) Run(ctx context.Context) {
	deliveryDone := make(chan struct{})
	go func() { a.deliver(ctx); close(deliveryDone) }()
	defer func() { <-deliveryDone }()
	collector := &Collector{}
	for {
		a.mu.Lock()
		cfg, err := a.Store.Settings()
		if err == nil {
			a.snapshot = collector.Collect(cfg.Mounts)
			var states map[string]AlertState
			states, err = a.Store.States()
			if err == nil {
				next, events := Evaluate(a.snapshot, cfg, states)
				err = a.Store.CommitCycle(next, events, cfg, a.snapshot.At)
			}
		}
		a.cycleError = ""
		if err != nil {
			a.cycleError = "Ошибка чтения или записи состояния"
			log.Printf("monitor cycle: %v", err)
		} else {
			a.lastCycle = time.Now()
		}
		interval := cfg.Interval
		if interval < 5 {
			interval = 15
		}
		a.mu.Unlock()
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (a *App) deliver(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		a.mu.Lock()
		cfg, err := a.Store.Settings()
		if err == nil {
			for _, ch := range []string{"telegram", "email"} {
				on := cfg.Telegram
				if ch == "email" {
					on = cfg.Email
				}
				if !cfg.Enabled || time.Now().Before(cfg.PausedUntil) || !on {
					err = a.Store.CancelPending(ch)
					if err != nil {
						break
					}
				}
			}
		}
		var d *Delivery
		if err == nil {
			d, err = a.Store.Due(time.Now())
		}
		a.mu.Unlock()
		if err != nil {
			log.Printf("outbox: %v", err)
			continue
		}
		if d == nil {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		sendErr := a.Notifier.Send(sendCtx, d.Channel, d.Message)
		cancel()
		if err = a.Store.Finish(*d, sendErr, time.Now()); err != nil {
			log.Printf("outbox result: %v", err)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		apiError(w, 415, "Ожидается application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		apiError(w, 400, "Некорректный JSON")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		apiError(w, 400, "Некорректный JSON")
		return false
	}
	return true
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.authorized(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("watchdog_session")
		a.mu.Lock()
		delete(a.sessions, sha256.Sum256([]byte(c.Value)))
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "watchdog_session", Path: "/", HttpOnly: true, Secure: a.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /api/status", a.authorized(a.status))
	mux.HandleFunc("GET /api/settings", a.authorized(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		cfg, err := a.Store.Settings()
		if err != nil {
			apiError(w, 500, "Ошибка базы данных")
			return
		}
		writeJSON(w, 200, cfg)
	}))
	mux.HandleFunc("PUT /api/settings", a.authorized(a.saveSettings))
	mux.HandleFunc("POST /api/test", a.authorized(a.testChannel))
	mux.HandleFunc("GET /api/history", a.authorized(func(w http.ResponseWriter, r *http.Request) {
		events, deliveries, err := a.Store.History()
		if err != nil {
			apiError(w, 500, "Ошибка базы данных")
			return
		}
		writeJSON(w, 200, map[string]any{"events": events, "deliveries": deliveries})
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		cfg, err := a.Store.Settings()
		healthy := err == nil && !a.lastCycle.IsZero() && time.Since(a.lastCycle) < time.Duration(cfg.Interval*3+30)*time.Second && a.cycleError == "" && len(a.snapshot.Errors) == 0
		if !healthy {
			apiError(w, 503, "not ready")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	web, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != "GET" && r.Header.Get("X-Watchdog-Request") != "1" {
				apiError(w, 403, "Отсутствует заголовок защиты запроса")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host && origin != "http://"+r.Host {
				apiError(w, 403, "Запрос с другого сайта запрещён")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *App) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("watchdog_session")
		if err != nil {
			apiError(w, 401, "Требуется вход")
			return
		}
		key := sha256.Sum256([]byte(cookie.Value))
		a.mu.Lock()
		s, ok := a.sessions[key]
		if ok && time.Now().After(s.Expires) {
			delete(a.sessions, key)
			ok = false
		}
		a.mu.Unlock()
		if !ok {
			apiError(w, 401, "Требуется вход")
			return
		}
		next(w, r)
	}
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := time.Now()
	a.mu.Lock()
	for k, v := range a.attempts {
		if now.After(v.Until) {
			delete(a.attempts, k)
		}
	}
	attempt := a.attempts[ip]
	if attempt.Count >= 5 || len(a.attempts) > 10000 {
		a.mu.Unlock()
		apiError(w, 429, "Слишком много попыток. Повторите через 15 минут.")
		return
	}
	if attempt.Count == 0 {
		attempt.Until = now.Add(15 * time.Minute)
	}
	attempt.Count++
	a.attempts[ip] = attempt
	a.mu.Unlock()
	if len(input.Password) > 72 || bcrypt.CompareHashAndPassword(a.PasswordHash, []byte(input.Password)) != nil {
		apiError(w, 401, "Неверный пароль")
		return
	}
	var token [32]byte
	if _, err = rand.Read(token[:]); err != nil {
		apiError(w, 500, "Не удалось создать сессию")
		return
	}
	value := hex.EncodeToString(token[:])
	a.mu.Lock()
	delete(a.attempts, ip)
	for k, v := range a.sessions {
		if now.After(v.Expires) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= 100 {
		a.mu.Unlock()
		apiError(w, 429, "Слишком много активных сессий")
		return
	}
	a.sessions[sha256.Sum256([]byte(value))] = session{now.Add(12 * time.Hour)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "watchdog_session", Value: value, Path: "/", HttpOnly: true, Secure: a.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	states, err := a.Store.States()
	if err != nil {
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	writeJSON(w, 200, map[string]any{"snapshot": a.snapshot, "states": states, "error": a.cycleError, "channels": map[string]bool{"telegram": a.Notifier.Ready("telegram"), "email": a.Notifier.Ready("email")}})
}
func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var cfg Settings
	if !readJSON(w, r, &cfg) {
		return
	}
	if err := cfg.Validate(); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if (cfg.Telegram && !a.Notifier.Ready("telegram")) || (cfg.Email && !a.Notifier.Ready("email")) {
		apiError(w, 400, "Сначала настройте секреты выбранного канала в окружении сервиса")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.Store.SaveSettings(cfg); err != nil {
		apiError(w, 500, "Ошибка сохранения")
		return
	}
	writeJSON(w, 200, cfg)
}
func (a *App) testChannel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Channel string `json:"channel"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !a.Notifier.Ready(input.Channel) {
		apiError(w, 400, "Канал не настроен")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, err := a.Store.Settings()
	if err != nil {
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	on := cfg.Telegram
	if input.Channel == "email" {
		on = cfg.Email
	}
	if !cfg.Enabled || time.Now().Before(cfg.PausedUntil) || !on {
		apiError(w, 400, "Для теста включите уведомления и канал, снимите паузу и сохраните настройки")
		return
	}
	var count int
	if err = a.Store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE metric='test' AND at>?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)).Scan(&count); err != nil {
		apiError(w, 500, "Ошибка базы данных")
		return
	}
	if count >= 3 {
		apiError(w, 429, "Не более трёх тестов в минуту")
		return
	}
	if err = a.Store.TestEvent(input.Channel, time.Now().UTC()); err != nil {
		apiError(w, 500, "Не удалось поставить сообщение в очередь")
		return
	}
	writeJSON(w, 202, map[string]string{"message": "Сообщение поставлено в очередь. Результат появится в истории."})
}

func ValidatePasswordHash(hash []byte) error {
	cost, err := bcrypt.Cost(hash)
	if err != nil || cost < 10 || cost > 16 {
		return errors.New("WATCHDOG_PASSWORD_HASH должен содержать bcrypt-хеш со стоимостью от 10 до 16")
	}
	return nil
}
