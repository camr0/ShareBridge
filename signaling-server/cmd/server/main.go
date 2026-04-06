package main

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/db"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
)

func main() {
	cfg := config.Load()
	h := hub.New()

	// Initialize database
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	// Create repositories
	apiKeyRepo := db.NewAPIKeyRepo(database)
	sessionRepo := db.NewSessionRepo(database)

	// Start background cleanup for expired sessions
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if err := sessionRepo.DeleteExpired(); err != nil {
				log.Printf("error cleaning expired sessions: %v", err)
			}
			<-ticker.C
		}
	}()

	r := gin.Default()
	handler.RegisterRoutes(r, h, cfg, apiKeyRepo, sessionRepo)

	// Log startup info
	adminStatus := "disabled"
	if cfg.AdminToken != "" {
		adminStatus = "enabled"
	}
	log.Printf("signaling server listening on :%s", cfg.Port)
	log.Printf("database: %s", cfg.DBPath)
	log.Printf("admin endpoints: %s", adminStatus)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
