package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

func RewriteHostCandidate(sdp string, newPort int) (string, string, int, error) {
	lines := strings.Split(sdp, "\r\n")
	out := make([]string, 0, len(lines))
	origIP := ""
	origPort := 0
	rewritten := false
	for _, line := range lines {
		if strings.HasPrefix(line, "a=candidate:") {
			if !rewritten {
				fields := strings.Fields(line)
				if len(fields) >= 8 && fields[2] == "udp" && fields[7] == "host" && isIPv4(fields[4]) {
					port, err := strconv.Atoi(fields[5])
					if err != nil {
						return "", "", 0, fmt.Errorf("parse candidate port: %w", err)
					}
					origIP = fields[4]
					origPort = port
					fields[4] = "127.0.0.1"
					fields[5] = strconv.Itoa(newPort)
					out = append(out, strings.Join(fields, " "))
					rewritten = true
					continue
				}
			}
			continue
		}
		out = append(out, line)
	}
	if !rewritten {
		return "", "", 0, fmt.Errorf("no IPv4 host candidate found")
	}
	return strings.Join(out, "\r\n"), origIP, origPort, nil
}
