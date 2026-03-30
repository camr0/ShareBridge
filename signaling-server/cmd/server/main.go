package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/handler"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func main() {
	cfg := config.Load()
	sessions := session.NewManager()
	h := hub.New()

	r := gin.Default()
	handler.RegisterRoutes(r, sessions, h, cfg)

	log.Printf("signaling server listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
