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
	debugFlag := flag.Bool("debug", false, "enable parsed Opera request diagnostics")
	wireDebugFlag := flag.Bool("wire-debug", false, "enable bounded hexadecimal protocol dumps")
	om4ReferenceFlag := flag.String("om4-reference-url", "", "forward OM4 requests to a compatible reference endpoint")
	om4CorpusFlag := flag.String("om4-corpus-dir", "", "save decrypted OM4 request/response pairs for renderer research")
	om4WelcomeTemplateFlag := flag.String("om4-welcome-template", "", "local OM4 first-time response frame template")
	flag.Parse()
	if *debugFlag {
		_ = os.Setenv("OMS_HTTP_DEBUG", "1")
	}
	if *wireDebugFlag {
		_ = os.Setenv("OMS_WIRE_DEBUG", "1")
	}
	if *om4ReferenceFlag != "" {
		_ = os.Setenv("OMS_OM4_REFERENCE_URL", *om4ReferenceFlag)
	}
	if *om4CorpusFlag != "" {
		_ = os.Setenv("OMS_OM4_CORPUS_DIR", *om4CorpusFlag)
	}
	if *om4WelcomeTemplateFlag != "" {
		_ = os.Setenv("OMS_OM4_WELCOME_TEMPLATE", *om4WelcomeTemplateFlag)
	}

	oms.ProxyCookieJarStore = proxy.CookieJarStoreInstance
	oms.ProxyDeriveClientKey = proxy.DeriveUpstreamClientKey

	addr := *addrFlag
	if env := os.Getenv("PORT"); env != "" {
		addr = "127.0.0.1:" + env
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)
	if *debugFlag || *wireDebugFlag {
		log.Printf("Debug enabled: parsed=%t wire=%t", *debugFlag, *wireDebugFlag)
	}

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
