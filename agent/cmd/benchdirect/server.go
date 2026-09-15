package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strconv"
)

//go:embed web
var webFS embed.FS

type answerMsg struct {
	Index int
	SDP   string
}

type benchServer struct {
	offers   []string
	answerCh chan answerMsg
	fs       fs.FS
	mode     string
	size     int64
	chunk    int
}

func newBenchServer(offers []string, answerCh chan answerMsg, fs fs.FS) *benchServer {
	return &benchServer{offers: offers, answerCh: answerCh, fs: fs}
}

func (s *benchServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": len(s.offers), "mode": s.mode, "size": s.size, "chunk": s.chunk,
		})
	})
	mux.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(r.URL.Query().Get("i"))
		if err != nil || i < 0 || i >= len(s.offers) {
			http.Error(w, "bad offer index", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"offer": s.offers[i]})
	})
	mux.HandleFunc("/answer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Index int    `json:"i"`
			SDP   string `json:"sdp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Index < 0 || body.Index >= len(s.offers) {
			http.Error(w, "bad answer index", http.StatusBadRequest)
			return
		}
		select {
		case s.answerCh <- answerMsg{Index: body.Index, SDP: body.SDP}:
		default:
		}
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
