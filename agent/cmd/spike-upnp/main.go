package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sharebridge/agent/internal/direct"
)

func main() {
	var (
		internalPort = flag.Int("internal-port", 8443, "local port the spike listens on")
		extPort      = flag.Int("ext-port", 443, "preferred external port (falls back to a high port if occupied)")
		lease        = flag.Int("lease", 300, "port mapping lease in seconds")
		serveHTTPS   = flag.Bool("https", false, "serve HTTPS with a self-signed cert (smoke test only)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mapper, err := direct.MapperForRouter(ctx)
	if err != nil {
		log.Fatalf("no mapper: %v", err)
	}
	ipStr, err := mapper.ExternalIP()
	if err != nil {
		log.Fatalf("external IP: %v", err)
	}

	// Spec §6: never clobber an existing (foreign) mapping; prefer 443, fall
	// back to a high dynamic-range port when 443 is taken.
	requested, err := direct.ChooseExternalPort(mapper, *extPort)
	if err != nil {
		log.Fatalf("choose external port: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *internalPort))
	if err != nil {
		log.Fatalf("listen :%d: %v", *internalPort, err)
	}
	defer ln.Close()

	// NAT-PMP may grant a different external port than requested; the returned
	// value is the endpoint to advertise.
	granted, err := mapper.AddPortMapping(requested, *internalPort, "sharebridge-spike", *lease)
	if err != nil {
		log.Fatalf("AddPortMapping: %v", err)
	}
	log.Printf("MAPPED external=%s:%d -> local :%d (lease %ds)", ipStr, granted, *internalPort, *lease)

	// Remove the mapping on SIGINT/SIGTERM as well as on normal return, and
	// only if this agent created it (§6).
	defer func() {
		if err := direct.DeleteOwnedMapping(mapper, granted); err != nil {
			log.Printf("DeleteOwnedMapping(%d): %v", granted, err)
		}
	}()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			// ~1 MiB of deterministic data, so the smoke test can prove a real
			// download (and later, that Range/throughput work).
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="spike.bin"`)
			w.Write(bytes.Repeat([]byte("sharebridge-spike\n"), 1<<16))
			return
		}
		fmt.Fprintf(w, "reachable via %s:%d\n", ipStr, granted)
	})

	var srv *http.Server
	if *serveHTTPS {
		cert, err := selfSignedCert(net.ParseIP(ipStr))
		if err != nil {
			log.Fatalf("self-signed cert: %v", err)
		}
		srv = &http.Server{
			Handler:   handler,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		}
		log.Printf("serving HTTPS (self-signed; smoke test only). Probe from off-LAN: https://%s:%d", ipStr, granted)
	} else {
		srv = &http.Server{Handler: handler}
		log.Printf("serving HTTP. Probe from off-LAN: http://%s:%d", ipStr, granted)
	}

	serveErr := make(chan error, 1)
	go func() {
		if *serveHTTPS {
			serveErr <- srv.ServeTLS(ln, "", "")
		} else {
			serveErr <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-serveErr:
		log.Printf("serve: %v", err)
	case <-ctx.Done():
		log.Println("SIGINT/SIGTERM received; removing port mapping")
	}
}

// selfSignedCert builds a throwaway self-signed certificate for the public IP,
// used ONLY for the Phase 0 smoke test. It is never served to real users; the
// production certificate is issued by a public ACME CA (spec §8).
func selfSignedCert(ip net.IP) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: ip.String()},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}
