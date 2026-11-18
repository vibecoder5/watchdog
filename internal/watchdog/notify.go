package watchdog

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strings"
	"time"
)

type Notifier struct {
	Token, ChatID                                                          string
	SMTPHost, SMTPPort, SMTPUser, SMTPPassword, SMTPFrom, SMTPTo, SMTPMode string
	Client                                                                 *http.Client
}

func EnvNotifier() *Notifier {
	return &Notifier{Token: os.Getenv("WATCHDOG_TELEGRAM_TOKEN"), ChatID: os.Getenv("WATCHDOG_TELEGRAM_CHAT_ID"), SMTPHost: os.Getenv("WATCHDOG_SMTP_HOST"), SMTPPort: os.Getenv("WATCHDOG_SMTP_PORT"), SMTPUser: os.Getenv("WATCHDOG_SMTP_USER"), SMTPPassword: os.Getenv("WATCHDOG_SMTP_PASSWORD"), SMTPFrom: os.Getenv("WATCHDOG_SMTP_FROM"), SMTPTo: os.Getenv("WATCHDOG_SMTP_TO"), SMTPMode: os.Getenv("WATCHDOG_SMTP_MODE"), Client: &http.Client{Timeout: 15 * time.Second}}
}
func (n *Notifier) Ready(channel string) bool {
	if channel == "telegram" {
		return n.Token != "" && n.ChatID != ""
	}
	if channel != "email" || n.SMTPHost == "" || n.SMTPPort == "" || (n.SMTPMode != "starttls" && n.SMTPMode != "tls") {
		return false
	}
	_, a := mail.ParseAddress(n.SMTPFrom)
	_, b := mail.ParseAddress(n.SMTPTo)
	return a == nil && b == nil && !strings.ContainsAny(n.SMTPFrom+n.SMTPTo, "\r\n")
}
func (n *Notifier) Send(ctx context.Context, channel, message string) error {
	if !n.Ready(channel) {
		return errors.New("канал не настроен: проверьте переменные окружения")
	}
	if channel == "telegram" {
		form := url.Values{"chat_id": {n.ChatID}, "text": {message}}
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+n.Token+"/sendMessage", strings.NewReader(form.Encode()))
		if err != nil {
			return errors.New("ошибка конфигурации Telegram")
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := n.Client.Do(req)
		if err != nil {
			return errors.New("Telegram недоступен или истекло время ожидания")
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Telegram: HTTP %d", resp.StatusCode)
		}
		return nil
	}
	if err := n.sendMail(ctx, message); err != nil {
		return errors.New("ошибка SMTP: проверьте TLS, учётные данные и доступность сервера")
	}
	return nil
}
func (n *Notifier) sendMail(ctx context.Context, message string) error {
	address := net.JoinHostPort(n.SMTPHost, n.SMTPPort)
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	tlsConfig := &tls.Config{ServerName: n.SMTPHost, MinVersion: tls.VersionTLS12}
	if n.SMTPMode == "tls" {
		secure := tls.Client(conn, tlsConfig)
		if err = secure.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = secure
	}
	client, err := smtp.NewClient(conn, n.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if n.SMTPMode == "starttls" {
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if n.SMTPUser != "" {
		if err = client.Auth(smtp.PlainAuth("", n.SMTPUser, n.SMTPPassword, n.SMTPHost)); err != nil {
			return err
		}
	}
	from, _ := mail.ParseAddress(n.SMTPFrom)
	to, _ := mail.ParseAddress(n.SMTPTo)
	if err = client.Mail(from.Address); err != nil {
		return err
	}
	if err = client.Rcpt(to.Address); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", from.String(), to.String(), mime.QEncoding.Encode("utf-8", "Watchdog: состояние сервера"), strings.ReplaceAll(message, "\n", "\r\n"))
	if err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
