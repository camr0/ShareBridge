package main

import (
	"encoding/json"
	"fmt"
	"io"
)

func writeJSON(w io.Writer, r rawResult) error {
	return json.NewEncoder(w).Encode(r)
}

func humanSummary(r rawResult) string {
	return fmt.Sprintf("mode=%s rtt=%dms sent=%d received=%d throughput=%.2f Mbps",
		r.Mode, r.RTT, r.SentBytes, r.Received, r.Mbps)
}
