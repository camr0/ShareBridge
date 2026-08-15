package main

import (
	"crypto/x509"
	"flag"
	"log"
	"os"

	"sharebridge/agent/internal/cert"
)

// Agent half: two modes.
//
//	(default) generate the key+CSR locally (key never leaves) and write them.
//	-validate: validate chain.pem against the EXISTING key (never regenerates,
//	           so the chain is checked against the key it was issued for).
func main() {
	ns := os.Getenv("SPIKE_NS")     // agent namespace, e.g. v7q4km2x9pz6dn3w
	base := os.Getenv("SPIKE_BASE") // base domain, e.g. sbx.example.com
	validateOnly := flag.Bool("validate", false, "validate chain.pem against the existing key")
	flag.Parse()
	if ns == "" || base == "" {
		log.Fatal("set SPIKE_NS and SPIKE_BASE")
	}

	if *validateOnly {
		keyPEM, err := os.ReadFile("key.pem")
		if err != nil {
			log.Fatalf("read key.pem: %v (run without -validate first to generate)", err)
		}
		chain, err := os.ReadFile("chain.pem")
		if err != nil {
			log.Fatalf("read chain.pem: %v", err)
		}
		if err := cert.ValidateChain(chain, keyPEM, ns, base, loadRoots()); err != nil {
			log.Fatalf("ValidateChain rejected returned chain: %v", err)
		}
		log.Printf("returned chain validated: exact SANs, key matches, chain verifies")
		return
	}

	keyPEM, csrPEM, err := cert.GenerateWildcardCSR(ns, base)
	if err != nil {
		log.Fatalf("GenerateWildcardCSR: %v", err)
	}
	if err := os.WriteFile("key.pem", keyPEM, 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("csr.pem", csrPEM, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("agent key + CSR written: key.pem (0600), csr.pem")
}

// loadRoots returns the trust root the agent validates against. For the spike,
// pass the ACME STAGING root PEM via SPIKE_CA_ROOT (the staging root is not in
// the system trust store). In production the agent pins the configured CA root.
func loadRoots() *x509.CertPool {
	if caPath := os.Getenv("SPIKE_CA_ROOT"); caPath != "" {
		pemBytes, err := os.ReadFile(caPath)
		if err != nil {
			log.Fatalf("read SPIKE_CA_ROOT: %v", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			log.Fatal("no certs in SPIKE_CA_ROOT")
		}
		return roots
	}
	if sys, err := x509.SystemCertPool(); err == nil {
		return sys
	}
	return x509.NewCertPool()
}
