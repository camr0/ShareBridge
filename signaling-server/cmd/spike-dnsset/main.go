// Command spike-dnsset creates or deletes a single A record — a one-off helper
// for the e2e test. The full create/update/delete + propagation loop is
// already validated by spike-ddns; this only persists a record for the duration
// of a manual browser test.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"sharebridge/server/internal/ddns"
)

func main() {
	var (
		delete = flag.Bool("delete", false, "delete the record instead of creating/updating it")
		name   = flag.String("name", "", "record name, e.g. *.ns.sharebridgeusercontent.com")
		ip     = flag.String("ip", "", "record content (IP address)")
		ttl    = flag.Int("ttl", 60, "record TTL in seconds")
	)
	flag.Parse()

	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	zone := os.Getenv("CLOUDFLARE_ZONE")
	if token == "" || zone == "" || *name == "" {
		log.Fatal("set CLOUDFLARE_API_TOKEN, CLOUDFLARE_ZONE, and -name")
	}
	ctx := context.Background()

	c, err := ddns.New(ctx, token, zone)
	if err != nil {
		log.Fatalf("ddns.New: %v", err)
	}

	if *delete {
		if err := c.DeleteA(ctx, *name); err != nil {
			log.Fatalf("DeleteA: %v", err)
		}
		log.Printf("deleted %s", *name)
		return
	}
	if *ip == "" {
		log.Fatal("set -ip for create/update")
	}
	id, err := c.UpsertA(ctx, *name, *ip, *ttl)
	if err != nil {
		log.Fatalf("UpsertA: %v", err)
	}
	log.Printf("upserted %s -> %s (id=%s, ttl=%d)", *name, *ip, id, *ttl)
}
