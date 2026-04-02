package turn

// ICEConfigRequest contains the parameters for building ICE server config.
type ICEConfigRequest struct {
	STUNURL     string       // STUN server URL
	TurnURL     string       // TURN server URL (empty if not configured)
	Credentials *Credentials // TURN credentials (nil if TURN not configured)
}

// ICEServer represents an ICE server configuration for JSON serialization.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// BuildICEConfig creates a slice of ICE servers for WebRTC configuration.
// If TURN is configured, includes both STUN and TURN with credentials.
func BuildICEConfig(cfg *ICEConfigRequest) []ICEServer {
	servers := []ICEServer{
		{URLs: []string{cfg.STUNURL}},
	}

	if cfg.TurnURL != "" && cfg.Credentials != nil {
		servers = append(servers, ICEServer{
			URLs:       []string{cfg.TurnURL},
			Username:   cfg.Credentials.Username,
			Credential: cfg.Credentials.Credential,
		})
	}

	return servers
}
