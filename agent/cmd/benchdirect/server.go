package main

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
)

type benchServer struct {
	offerFn  func() string
	answerCh chan string
	fs       fs.FS
	mode     string
	size     int64
	chunk    int
}

func newBenchServer(offerFn func() string, answerCh chan string, fs fs.FS) *benchServer {
	return &benchServer{offerFn: offerFn, answerCh: answerCh, fs: fs}
}

func (s *benchServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"offer": s.offerFn(), "mode": s.mode, "size": s.size, "chunk": s.chunk,
		})
	})
	mux.HandleFunc("/answer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SDP string `json:"sdp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.answerCh <- body.SDP
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", http.FileServer(http.FS(s.fs)))
	return mux
}

func (s *benchServer) listen() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go http.Serve(ln, s.handler())
	return "http://" + ln.Addr().String(), nil
}
