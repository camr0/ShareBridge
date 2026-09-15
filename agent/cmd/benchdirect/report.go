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
	s := fmt.Sprintf("mode=%s conns=%d sharing=%s rtt=%dms jitter=%dms bw=%d loss=%v sent=%d received=%d throughput=%.2f Mbps wall=%.2f Mbps chrome_cores=%.2f go_cpu=%.2fs",
		r.Mode, r.Conns, r.Sharing, r.RTT, r.JitterMs, r.Bandwidth, r.Loss,
		r.SentBytes, r.Received, r.Mbps, r.WallMbps, r.ChromeCores, r.GoCPUSec)
	if r.Err != "" {
		s += " error=" + r.Err
	}
	return s
}
