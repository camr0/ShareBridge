package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"sort"
	"time"
)

// selfSignedCert builds an in-memory self-signed ECDSA P-256 cert for the
// loopback harness. It models the agent's TLS termination (kernel TCP + TLS,
// same record overhead) without a public CA; it is never served to real users.
func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "benchdirect-local"},
		DNSNames:     []string{"127.0.0.1", "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
}

func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

type sizeReader struct {
	b   []byte
	off int64
}

func (r *sizeReader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += int64(n)
	return n, nil
}
func (r *sizeReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = offset
	case io.SeekCurrent:
		r.off += offset
	case io.SeekEnd:
		r.off = int64(len(r.b)) + offset
	}
	return r.off, nil
}

// serveFile runs the HTTPS data-plane server: a deterministic in-memory
// size-byte file served via http.ServeContent (real Range/206) over TLS on a
// plain TCP listener — exactly the agent's data-plane semantics (kernel TCP +
// TLS + Range). Returns the base URL.
func serveFile(ctx context.Context, size int64, host string) (string, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	// Allocate the payload ONCE and share it across requests. The buffer is
	// immutable (never written); each request gets its own sizeReader with its
	// own offset, preserving http.ServeContent Range behavior without a fresh
	// size-byte zeroed allocation per request.
	data := make([]byte, size)
	mux.HandleFunc("/bench.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "bench.bin", time.Time{}, &sizeReader{b: data})
	})
	srv := &http.Server{
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return "https://" + ln.Addr().String(), nil
}

// runHTTPServer is the --mode https dispatch: serve size bytes on all
// interfaces so a remote fetch client can reach it, print the URL, and block
// until the signal context is cancelled.
func runHTTPServer(ctx context.Context, cfg runConfig) error {
	url, err := serveFile(ctx, cfg.size, "0.0.0.0")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "serving %d bytes at %s/bench.bin (substitute 0.0.0.0 with this host's reachable address)\n", cfg.size, url)
	<-ctx.Done()
	return nil
}

type fetchResult struct {
	Mode      string    `json:"mode"`
	RTT       int       `json:"rtt_ms"`
	Requested int64     `json:"requested_bytes"`
	Received  int64     `json:"received_bytes"`
	Mbps      float64   `json:"mbps"`
	Samples   []float64 `json:"samples_mbps"` // per-100ms throughput
	Stalls    int       `json:"stalls"`       // ≥2s runs of <1 Mbps
}

// fetch downloads url with a fresh TCP+TLS connection each rep (no keep-alive,
// so every rep models a fresh recipient connection), measuring per-100ms
// throughput windows. The FIRST rep is a warm-up (TLS handshake + slow-start)
// and is excluded from the returned slice.
func fetch(ctx context.Context, url string, size int64, rttMs int, reps int) ([]fetchResult, error) {
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   insecureTLSConfig(),
		DisableKeepAlives: true,
	}}
	run := func() (fetchResult, error) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fetchResult{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fetchResult{}, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fetchResult{}, fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode)
		}
		samples, received, err := measureCopy(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fetchResult{}, err
		}
		if received != size {
			return fetchResult{}, fmt.Errorf("GET %s: received %d bytes, want %d", url, received, size)
		}
		return fetchResult{
			Mode: "https", RTT: rttMs, Requested: size, Received: received,
			Mbps:    float64(received) * 8 / time.Since(start).Seconds() / 1e6,
			Samples: samples, Stalls: countStalls(samples),
		}, nil
	}
	if _, err := run(); err != nil { // warm-up, discarded
		return nil, err
	}
	out := make([]fetchResult, 0, reps)
	for i := 0; i < reps; i++ {
		r, err := run()
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func measureCopy(r io.Reader) ([]float64, int64, error) {
	buf := make([]byte, 128*1024)
	type readResult struct {
		n   int
		err error
	}
	reads := make(chan readResult, 1)
	go func() {
		for {
			n, err := r.Read(buf)
			reads <- readResult{n, err}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var total int64
	var winBytes int64
	winStart := time.Now()
	var samples []float64

	for {
		select {
		case rr := <-reads:
			total += int64(rr.n)
			winBytes += int64(rr.n)
			if rr.err != nil {
				// Finalize the in-flight window (only if it carried bytes, to
				// avoid a spurious trailing zero).
				if el := time.Since(winStart); el > 0 && winBytes > 0 {
					samples = append(samples, float64(winBytes)*8/el.Seconds()/1e6)
				}
				if rr.err == io.EOF {
					return samples, total, nil
				}
				return samples, total, rr.err
			}
		case <-ticker.C:
			// Emit a sample every 100ms regardless of whether bytes arrived,
			// so a full stall (Read blocked, zero bytes) yields consecutive
			// zero-throughput windows that countStalls can detect.
			el := time.Since(winStart)
			samples = append(samples, float64(winBytes)*8/el.Seconds()/1e6)
			winStart = time.Now()
			winBytes = 0
		}
	}
}

// countStalls counts ≥2s runs where every 100ms window is <1 Mbps (the SCTP
// collapse signature: multi-second stalls). Direct-TCP must show zero.
func countStalls(samples []float64) int {
	stalls, run := 0, 0
	for _, s := range samples {
		if s < 1.0 {
			run++
		} else {
			run = 0
		}
		if run == 20 { // 20 × 100ms = 2s
			stalls++
		}
	}
	return stalls
}

// summarize returns median, min, max, and p95 Mbps across reps.
func summarize(rs []fetchResult) (median, min, max, p95 float64) {
	m := make([]float64, len(rs))
	for i, r := range rs {
		m[i] = r.Mbps
	}
	sort.Float64s(m)
	min, max = m[0], m[len(m)-1]
	median = m[len(m)/2]
	p95 = m[int(0.95*float64(len(m)-1))]
	return
}
