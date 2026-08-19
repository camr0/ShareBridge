package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/miekg/dns"

	"sharebridge/control/internal/ddns"
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

	// 2. Authoritative propagation: query the zone's own nameservers directly
	//    until they serve the new IP. Failure here is fatal.
	authElapsed, err := resolveAuthoritative(zone, probe, ip, 5*time.Minute)
	if err != nil {
		log.Fatalf("authoritative: %v", err)
	}
	log.Printf("authoritative propagation confirmed in %s", authElapsed)

	// 3. Recursive propagation: poll public resolvers until they see the IP.
	recElapsed, err := waitRecursive(probe, ip, 5*time.Minute)
	if err != nil {
		log.Fatalf("recursive: %v", err)
	}
	log.Printf("recursive propagation confirmed in %s", recElapsed)

	// 4. Update: point at a second IP and confirm BOTH the authoritative
	//    servers and the recursive resolver see the change.
	if ip2 := os.Getenv("SPIKE_DDNS_IP2"); ip2 != "" && ip2 != ip {
		if _, err := c.UpsertA(ctx, name, ip2, 60); err != nil {
			log.Fatalf("update UpsertA: %v", err)
		}
		authElapsed, err := resolveAuthoritative(zone, probe, ip2, 5*time.Minute)
		if err != nil {
			log.Fatalf("authoritative after update: %v", err)
		}
		log.Printf("authoritative after update confirmed in %s", authElapsed)
		recElapsed, err := waitRecursive(probe, ip2, 5*time.Minute)
		if err != nil {
			log.Fatalf("recursive after update: %v", err)
		}
		log.Printf("recursive after update confirmed in %s", recElapsed)
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
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("record %s still resolves after %s", name, timeout)
}

// recursiveResolver is the recursive DNS resolver used to discover the zone's
// authoritative nameservers. It's a variable so tests can point it at a local
// in-process DNS server.
var recursiveResolver = "1.1.1.1:53"

// authoritativeAddr returns the dial address for one authoritative nameserver.
// It's a variable so tests can redirect authoritative queries to a local
// in-process DNS server without relying on the system resolver.
var authoritativeAddr = func(ns string) string { return dns.Fqdn(ns) + ":53" }

// pollInterval is the delay between propagation probes.
var pollInterval = 5 * time.Second

// resolveAuthoritative discovers the zone's NS records via a recursive
// resolver, then polls each authoritative server directly until every one
// serves wantIP for probe. It returns the elapsed time until confirmation and
// the first error that prevented confirmation (per-NS failures are no longer
// silently swallowed; they are folded into the returned error).
func resolveAuthoritative(zone, probe, wantIP string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(zone), dns.TypeNS)
	r, _, err := c.Exchange(m, recursiveResolver)
	if err != nil {
		return time.Since(start), fmt.Errorf("NS lookup error: %w", err)
	}

	var nameservers []string
	for _, ans := range r.Answer {
		if ns, ok := ans.(*dns.NS); ok {
			nameservers = append(nameservers, ns.Ns)
		}
	}
	if len(nameservers) == 0 {
		return time.Since(start), fmt.Errorf("no authoritative nameservers found for %s", zone)
	}

	for {
		var pending []string
		var lastErr error
		for _, ns := range nameservers {
			m2 := new(dns.Msg)
			m2.SetQuestion(dns.Fqdn(probe), dns.TypeA)
			r2, _, err := c.Exchange(m2, authoritativeAddr(ns))
			if err != nil {
				lastErr = err
				pending = append(pending, ns)
				continue
			}
			serving := false
			for _, a := range r2.Answer {
				if rec, ok := a.(*dns.A); ok && rec.A.String() == wantIP {
					serving = true
					break
				}
			}
			if !serving {
				pending = append(pending, ns)
			}
		}
		if len(pending) == 0 {
			return time.Since(start), nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return time.Since(start), fmt.Errorf("authoritative propagation did not confirm %s for %s within %s (last error: %v; pending: %v)", wantIP, probe, timeout, lastErr, pending)
			}
			return time.Since(start), fmt.Errorf("authoritative propagation did not confirm %s for %s within %s (pending: %v)", wantIP, probe, timeout, pending)
		}
		time.Sleep(pollInterval)
	}
}

// waitRecursive polls the system resolver (recursive) until it returns wantIP,
// returning the elapsed time until confirmation.
func waitRecursive(name, wantIP string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		if ips, err := net.LookupHost(name); err == nil {
			for _, ip := range ips {
				if ip == wantIP {
					return time.Since(start), nil
				}
			}
		}
		time.Sleep(pollInterval)
	}
	return time.Since(start), fmt.Errorf("recursive resolver did not return %s for %s within %s", wantIP, name, timeout)
}
