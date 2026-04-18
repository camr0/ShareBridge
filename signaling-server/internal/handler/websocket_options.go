package handler

import "github.com/coder/websocket"

func wsAcceptOptions() *websocket.AcceptOptions {
	return &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	}
}
