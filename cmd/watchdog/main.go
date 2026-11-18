package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vibecoder5/watchdog/internal/watchdog"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if len(os.Args) == 2 && os.Args[1] == "hash-password" {
		fmt.Fprintln(os.Stderr, "Введите пароль через stdin (12–72 байта), затем Enter:")
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 1024)).ReadString('\n')
		if err != nil && len(line) == 0 {
			log.Fatal("не удалось прочитать пароль")
		}
		password := strings.TrimRight(line, "\r\n")
		if len(password) < 12 || len(password) > 72 {
			log.Fatal("пароль должен содержать 12–72 байта")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(hash))
		return nil
	}
	if len(os.Args) > 1 {
		log.Fatal("использование: watchdog [hash-password]")
	}
	hash := []byte(os.Getenv("WATCHDOG_PASSWORD_HASH"))
	if err := watchdog.ValidatePasswordHash(hash); err != nil {
		log.Fatal(err)
	}
	address := env("WATCHDOG_LISTEN", "127.0.0.1:8080")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		log.Fatal("некорректный WATCHDOG_LISTEN")
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && os.Getenv("WATCHDOG_ALLOW_REMOTE") != "true" {
		log.Fatal("для внешнего bind требуется WATCHDOG_ALLOW_REMOTE=true и HTTPS reverse proxy")
	}
	data := env("WATCHDOG_DATA_DIR", "./data")
	if err = os.MkdirAll(data, 0700); err != nil {
		log.Fatal(err)
	}
	store, err := watchdog.Open(filepath.Join(data, "watchdog.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.DB.Close()
	cfg, err := store.Settings()
	if err != nil {
		log.Fatal(err)
	}
	if err = cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	app := watchdog.NewApp(store, watchdog.EnvNotifier(), hash, os.Getenv("WATCHDOG_SECURE_COOKIE") == "true")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	done := make(chan struct{})
	go func() { app.Run(workerCtx); close(done) }()
	server := &http.Server{Addr: address, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	log.Printf("watchdog listening on %s", address)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serveErrors:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
		server.Close()
		if err == nil {
			err = shutdownErr
		}
	}
	cancelWorker()
	<-done
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
