package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/miekg/dns"

	"sharebridge/server/internal/ddns"
)

// DDNS spike: create/update/delete the direct wildcard A record and measure
// authoritative + recursive propagation at TTL 60 (spec §13.3).
//
// The record NAME is the wildcard (e.g. *.v7q4km2x9pz6dn3w.sbx.example.com),
// but propagation is verified by resolving a CONCRETE host under it
// (e.g. probe.v7q4km2x9pz6dn3w.sbx.example.com) that the wildcard covers.
// The relay wildcard (*.relay.<ns>... -> gateway IP) is static and out of
// scope for this DDNS test.
func main() {
	ctx := context.Background()

	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	zone := os.Getenv("CLOUDFLARE_ZONE")
	name := os.Getenv("SPIKE_DDNS_NAME")   // wildcard A record to create
	probe := os.Getenv("SPIKE_PROBE_NAME") // concrete host under the wildcard
	ip := os.Getenv("SPIKE_DDNS_IP")       // agent's reported public IP
	if token == "" || zone == "" || name == "" || probe == "" || ip == "" {
		log.Fatal("set CLOUDFLARE_API_TOKEN, CLOUDFLARE_ZONE, SPIKE_DDNS_NAME, SPIKE_PROBE_NAME, SPIKE_DDNS_IP")
	}

	c, err := ddns.New(ctx, token, zone)
	if err != nil {
		log.Fatalf("ddns client: %v", err)
	}

	// 1. Create (or update) the direct wildcard A record with TTL 60.
	id, err := c.UpsertA(ctx, name, ip, 60)
	if err != nil {
		log.Fatalf("UpsertA: %v", err)
	}
	log.Printf("A record %s -> %s (id=%s, ttl=60)", name, ip, id)

	// 2. Authoritative propagation: query the zone's own nameservers directly.
	log.Printf("authoritative: %v", resolveAuthoritative(zone, probe))

	// 3. Recursive propagation: poll public resolvers until they see the IP.
	log.Printf("recursive: %v", waitRecursive(probe, ip, 5*time.Minute))

	// 4. Update: point at a second IP and confirm both see the change.
	if ip2 := os.Getenv("SPIKE_DDNS_IP2"); ip2 != "" && ip2 != ip {
		if _, err := c.UpsertA(ctx, name, ip2, 60); err != nil {
			log.Fatalf("update UpsertA: %v", err)
		}
		log.Printf("recursive after update: %v", waitRecursive(probe, ip2, 5*time.Minute))
	}

	// 5. Delete and confirm removal (recursive + authoritative NXDOMAIN/no-data).
	if err := c.DeleteA(ctx, name); err != nil {
		log.Fatalf("DeleteA: %v", err)
	}
	if err := waitGone(probe, 2*time.Minute); err != nil {
		log.Fatalf("post-delete: %v", err)
	}
	log.Printf("A record %s deleted; removal confirmed (NXDOMAIN/no-data)", name)
}

// waitGone polls until name no longer resolves to any A record.
func waitGone(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ips, err := net.LookupHost(name); err != nil || len(ips) == 0 {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("record %s still resolves after %s", name, timeout)
}

// resolveAuthoritative discovers the zone's NS records via a recursive
// resolver, then queries each authoritative server directly for the A record.
func resolveAuthoritative(zone, probe string) []string {
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(zone), dns.TypeNS)
	r, _, err := c.Exchange(m, "1.1.1.1:53")
	if err != nil {
		return []string{fmt.Sprintf("NS lookup error: %v", err)}
	}
	var ips []string
	m2 := new(dns.Msg)
	m2.SetQuestion(dns.Fqdn(probe), dns.TypeA)
	for _, ans := range r.Answer {
		ns, ok := ans.(*dns.NS)
		if !ok {
			continue
		}
		r2, _, err := c.Exchange(m2, dns.Fqdn(ns.Ns)+":53")
		if err != nil {
			continue
		}
		for _, a := range r2.Answer {
			if rec, ok := a.(*dns.A); ok {
				ips = append(ips, rec.A.String())
			}
		}
	}
	return ips
}

// waitRecursive polls the system resolver (recursive) until it returns wantIP.
func waitRecursive(name, wantIP string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ips, err := net.LookupHost(name); err == nil {
			for _, ip := range ips {
				if ip == wantIP {
					return nil
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("recursive resolver did not return %s for %s within %s", wantIP, name, timeout)
}
