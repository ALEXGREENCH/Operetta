package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"operetta/internal/proxy"
	"operetta/oms"
)

func main() {
	addrFlag := flag.String("addr", "127.0.0.1:8081", "listen address; use :8081 only for an intentionally public deployment")
	flag.Parse()

	oms.ProxyCookieJarStore = proxy.CookieJarStoreInstance
	oms.ProxyDeriveClientKey = proxy.DeriveUpstreamClientKey

	addr := *addrFlag
	if env := os.Getenv("PORT"); env != "" {
		addr = "127.0.0.1:" + env
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	handler := proxy.New(proxy.DefaultConfig())
	defer handler.Close()
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// Conservative timeouts to avoid slowloris and leaked connections blocking the server
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(os.Stdout, "HTTPERR ", log.LstdFlags|log.Lmicroseconds),
		ConnState: func(c net.Conn, s http.ConnState) {
			log.Printf("CONN %s %s", s.String(), c.RemoteAddr())
		},
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Listen error on %s: %v", addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Println("Listening on", addr)
		errCh <- srv.Serve(ln)
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server stopped: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}
}
