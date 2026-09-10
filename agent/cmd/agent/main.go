package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/daemon"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/web"
)

var (
	password string
)

func init() {
	rootCmd.AddCommand(daemonCmd)

	daemonCmd.Flags().StringVarP(&password, "password", "p", "", "Password for web UI (optional)")
}

var rootCmd = &cobra.Command{
	Use:   "sharebridge",
	Short: "ShareBridge agent for secure file sharing",
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the persistent daemon with web UI",
	RunE:  runDaemon,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runDaemon starts the persistent daemon with web UI.
func runDaemon(cmd *cobra.Command, args []string) error {
	// Create config manager
	cfgMgr, err := config.NewManager()
	if err != nil {
		return fmt.Errorf("create config manager: %w", err)
	}
	cfg := cfgMgr.Get()

	// Validate API key is set
	if cfg.APIKey == "" {
		return fmt.Errorf("API key required — set in config.json or SHAREBRIDGE_API_KEY env var")
	}

	// Fail closed before anything starts: an explicit non-loopback admin bind
	// without a credential would expose the admin surface unauthenticated.
	uiAddr := cfg.UIAddr
	uiPassword := cfg.UIPassword
	if uiPassword == "" {
		uiPassword = password // Allow CLI override
	}
	if err := config.ValidateAdminBind(uiAddr, uiPassword); err != nil {
		return err
	}

	// Create store
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	// Create daemon
	d, err := daemon.New(cfgMgr, st)
	if err != nil {
		return fmt.Errorf("create daemon: %w", err)
	}

	// Create web server
	uiPort := cfg.UIPort

	webServer, err := web.NewWebServer(nil, uiAddr, uiPort, uiPassword)
	if err != nil {
		return fmt.Errorf("create web server: %w", err)
	}

	// Set up circular reference
	d.SetWebServer(webServer)
	webServer.SetDaemon(d)

	// Handle Ctrl-C gracefully: the NotifyContext process context drives the
	// signaling loop, the relay tunnel supervision (manager + credential
	// requester, §7.4), and every other daemon goroutine.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Start daemon
	errChan := d.Start(ctx)

	log.Printf("daemon started — web UI at http://%s:%d", uiAddr, uiPort)

	// Wait for interrupt or error
	select {
	case <-ctx.Done():
		log.Println("shutting down...")
	case err := <-errChan:
		log.Printf("daemon error: %v", err)
	}

	// Cancel the process context BEFORE tearing down so the WebSocket read
	// loops, the tunnel credential-requester worker, and every other
	// context-driven goroutine end deterministically; d.Stop then shuts the
	// HTTPS listener, the frpc child (graceful → kill), and session resources
	// down in order.
	cancel()

	// Stop gracefully
	if err := d.Stop(); err != nil {
		log.Printf("stop error: %v", err)
	}

	return nil
}
