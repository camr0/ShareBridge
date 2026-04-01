package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/handler"
	"opencloudshare/server/internal/hub"
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

	r := gin.Default()
	handler.RegisterRoutes(r, h, cfg, database)

	log.Printf("signaling server listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
