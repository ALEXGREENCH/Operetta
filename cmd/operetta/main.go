package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"

	"operetta/internal/proxy"
)

func main() {
	addrFlag := flag.String("addr", ":8081", "listen address, e.g. :81 or 0.0.0.0:8081")
	flag.Parse()

	addr := *addrFlag
	if env := os.Getenv("PORT"); env != "" {
		addr = ":" + env
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	handler := proxy.NewServer()
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  0,
		WriteTimeout: 0,
		ErrorLog:     log.New(os.Stdout, "HTTPERR ", log.LstdFlags|log.Lmicroseconds),
		ConnState: func(c net.Conn, s http.ConnState) {
			log.Printf("CONN %s %s", s.String(), c.RemoteAddr())
		},
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Listen error on %s: %v", addr, err)
	}

	log.Println("Listening on", addr)
	log.Fatal(srv.Serve(ln))
}
