package watchdog

import (
	"bytes"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthSettingsAndSecurity(t *testing.T) {
	s := newTestStore(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password-123"), 10)
	if err != nil {
		t.Fatal(err)
	}
	a := NewApp(s, &Notifier{Token: "SECRET_BOT_TOKEN", ChatID: "SECRET_CHAT"}, hash, false)
	handler := a.Handler()
	request := func(method, path, body string, cookie *http.Cookie, header bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Content-Type", "application/json")
		if header {
			r.Header.Set("X-Watchdog-Request", "1")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/status", "", nil, true); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/api/login", `{"password":"test-password-123"}`, nil, false); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := request("POST", "/api/login", `{"password":"test-password-123"}`, nil, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	cookie := cookies[0]
	w = request("GET", "/api/status", "", cookie, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET") {
		t.Fatal(w.Code, w.Body.String())
	}
	cfg := Defaults()
	cfg.Telegram = true
	cfg.Interval = 30
	b, _ := json.Marshal(cfg)
	w = request("PUT", "/api/settings", string(b), cookie, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	loaded, _ := s.Settings()
	if loaded.Interval != 30 || !loaded.Telegram {
		t.Fatal(loaded)
	}
	w = request("POST", "/api/test", `{"channel":"telegram"}`, cookie, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if d, err := s.Due(time.Now()); err != nil || d == nil || d.Channel != "telegram" {
		t.Fatal(d, err)
	}
	w = request("POST", "/api/logout", `{}`, cookie, true)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", "/api/settings", "", cookie, true); w.Code != 401 {
		t.Fatal("session not revoked")
	}
	w = request("GET", "/", "", nil, true)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("watchdog")) {
		t.Fatal("embedded UI unavailable")
	}
}
func TestLoginLimitAndCSRF(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("test-password-123"), 10)
	a := NewApp(newTestStore(t), &Notifier{}, hash, true)
	handler := a.Handler()
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"wrong"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Watchdog-Request", "1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatal(i, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"test-password-123"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Watchdog-Request", "1")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin login allowed")
	}
}
