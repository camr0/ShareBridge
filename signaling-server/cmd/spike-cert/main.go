package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/go-acme/lego/v4/lego"

	"sharebridge/server/internal/certcoordinator"
)

// Control-plane half: read the agent's CSR, verify it authorizes the enrolling
// namespace, complete DNS-01 ACME issuance, write the chain. It never sees the
// agent's private key.
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	csrPEM, err := os.ReadFile(os.Getenv("CSR_FILE"))
	if err != nil {
		log.Fatalf("read CSR_FILE: %v", err)
	}
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	email := os.Getenv("ACME_EMAIL")
	ns := os.Getenv("SPIKE_NS")
	base := os.Getenv("SPIKE_BASE")
	if token == "" || email == "" || ns == "" || base == "" {
		log.Fatal("set CLOUDFLARE_API_TOKEN, ACME_EMAIL, SPIKE_NS, SPIKE_BASE")
	}
	// STAGING first: run the whole flow against the ACME staging CA before
	// touching production. Set ACME_CA to the production directory URL only
	// after staging succeeds end-to-end.
	ca := os.Getenv("ACME_CA")
	if ca == "" {
		ca = lego.LEDirectoryStaging
	}
	outPath := os.Getenv("CHAIN_FILE")
	if outPath == "" {
		outPath = "chain.pem"
	}

	chain, err := certcoordinator.CompleteCSR(ctx, csrPEM, certcoordinator.ACMEConfig{
		CA:              ca,
		Email:           email,
		CloudflareToken: token,
		Namespace:       ns,
		BaseDomain:      base,
	})
	if err != nil {
		log.Fatalf("CompleteCSR: %v", err)
	}
	if err := os.WriteFile(outPath, chain, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("chain written to %s (%d bytes) via %s", outPath, len(chain), ca)
}
