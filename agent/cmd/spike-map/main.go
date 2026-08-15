// Command spike-map maps an external port to a local internal port via
// UPnP/NAT-PMP and holds it until interrupted — a one-off helper to expose an
// already-running local server (e.g. the benchdirect HTTPS server) to the
// public internet for a throughput test. Throwaway; not production code.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"sharebridge/agent/internal/direct"
)

func main() {
	extPort := flag.Int("ext", 443, "preferred external port (falls back if occupied)")
	intPort := flag.Int("int", 8443, "internal port to map")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mapper, err := direct.MapperForRouter(ctx)
	if err != nil {
		log.Fatalf("mapper: %v", err)
	}
	ip, err := mapper.ExternalIP()
	if err != nil {
		log.Fatalf("external IP: %v", err)
	}
	requested, err := direct.ChooseExternalPort(mapper, *extPort)
	if err != nil {
		log.Fatalf("choose port: %v", err)
	}
	granted, err := mapper.AddPortMapping(requested, *intPort, "sharebridge-bench", 600)
	if err != nil {
		log.Fatalf("AddPortMapping: %v", err)
	}
	defer func() {
		if err := direct.DeleteOwnedMapping(mapper, granted); err != nil {
			log.Printf("delete mapping: %v", err)
		}
	}()
	log.Printf("mapped %s:%d -> local :%d (lease 600s)", ip, granted, *intPort)

	<-ctx.Done()
	log.Println("shutting down; removing mapping")
}
