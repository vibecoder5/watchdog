package watchdog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTelegramDeliveryAndSecretRedaction(t *testing.T) {
	n := &Notifier{Token: "SECRET_TOKEN", ChatID: "123", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" {
			t.Fatal(r.URL.Host)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("text") != "test" || r.Form.Get("chat_id") != "123" {
			t.Fatal(r.Form)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
	})}}
	if err := n.Send(context.Background(), "telegram", "test"); err != nil {
		t.Fatal(err)
	}
	n.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New(r.URL.String()) })
	err := n.Send(context.Background(), "telegram", "test")
	if err == nil || strings.Contains(err.Error(), "SECRET_TOKEN") {
		t.Fatal("secret leaked", err)
	}
	n.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("secret upstream body")), Header: http.Header{}}, nil
	})
	err = n.Send(context.Background(), "telegram", "test")
	if err == nil || strings.Contains(err.Error(), "upstream") {
		t.Fatal(err)
	}
}
func TestSMTPRequiresTLSAndValidHeaders(t *testing.T) {
	n := &Notifier{SMTPHost: "smtp.example.com", SMTPPort: "587", SMTPFrom: "a@example.com", SMTPTo: "b@example.com"}
	if n.Ready("email") {
		t.Fatal("plaintext SMTP accepted")
	}
	n.SMTPMode = "starttls"
	if !n.Ready("email") {
		t.Fatal("valid SMTP rejected")
	}
	n.SMTPTo = "b@example.com\r\nBcc: c@example.com"
	if n.Ready("email") {
		t.Fatal("header injection accepted")
	}
}
