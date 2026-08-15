// Command spike-e2e wires the validated direct-TCP building blocks — a real
// wildcard certificate, SNI/origin→share binding (direct.Binder), and the
// on-demand UPnP port (direct.OnDemandPort) — into a minimal HTTPS server that
// serves an HTML page and a file. It is a throwaway end-to-end proof that a
// browser can load a page P2P over direct HTTPS; not production code.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sharebridge/agent/internal/direct"
)

func main() {
	var (
		ns        = flag.String("ns", "", "agent namespace (e.g. sbe2eXXXX)")
		origin    = flag.String("origin", "", "origin hostname, e.g. demo.<ns>.sharebridgeusercontent.com")
		code      = flag.String("code", "", "share code to authorize (e.g. abc123)")
		keyFile   = flag.String("key", "key.pem", "TLS private key PEM path")
		chainFile = flag.String("chain", "chain.pem", "TLS chain PEM path")
		intPort   = flag.Int("internal-port", 8443, "local listen port")
		extPort   = flag.Int("ext-port", 443, "preferred external port (falls back if occupied)")
	)
	flag.Parse()
	if *ns == "" || *origin == "" || *code == "" {
		log.Fatal("set -ns, -origin, and -code")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1. Real certificate (issued for *.ns and *.relay.ns by the control plane).
	cert, err := tls.LoadX509KeyPair(*chainFile, *keyFile)
	if err != nil {
		log.Fatalf("load cert: %v", err)
	}

	// 2. SNI/origin→share binding.
	binder := direct.NewBinder(*ns)
	if err := binder.Allow(*origin, direct.RouteDirect, *code); err != nil {
		log.Fatalf("binder.Allow: %v", err)
	}

	// 3. On-demand public port (closed by default; opened once for the demo).
	mapper, err := direct.MapperForRouter(ctx)
	if err != nil {
		log.Fatalf("mapper: %v", err)
	}
	extIP, err := mapper.ExternalIP()
	if err != nil {
		log.Fatalf("external IP: %v", err)
	}
	requested, err := direct.ChooseExternalPort(mapper, *extPort)
	if err != nil {
		log.Fatalf("choose external port: %v", err)
	}
	port := direct.NewOnDemandPortOpts(mapper, requested, *intPort, 5*time.Minute)
	if err := port.OpenFor("e2e-demo", 5*time.Minute); err != nil {
		log.Fatalf("open port: %v", err)
	}
	defer port.Close()
	granted := port.GrantedPort()

	// 4. Serve a real page + a file over direct HTTPS. The Binder has already
	// authorized this request (the path carries /s/<code>), so we strip that
	// prefix and route on the remainder.
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/s/"+*code)
		if rest == "/file" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="e2e.bin"`)
			_, _ = w.Write([]byte("ShareBridge direct-TCP e2e: this file arrived P2P over HTTPS.\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><h1>Direct-TCP e2e works</h1><p>origin: %s</p><p>external: %s:%d</p><p><a href='/s/%s/file'>download file</a></p></body></html>", *origin, extIP, granted, *code)
	})

	tlsCfg := binder.TLSConfig()
	tlsCfg.Certificates = []tls.Certificate{cert}

	srv := &http.Server{
		Handler:   binder.Handler(page),
		TLSConfig: tlsCfg,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *intPort))
	if err != nil {
		log.Fatalf("listen :%d: %v", *intPort, err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()

	if granted == 443 {
		log.Printf("browse: https://%s/s/%s", *origin, *code)
	} else {
		log.Printf("browse: https://%s:%d/s/%s", *origin, granted, *code)
	}

	<-ctx.Done()
	log.Println("shutting down; removing port mapping")
	_ = srv.Close()
}
