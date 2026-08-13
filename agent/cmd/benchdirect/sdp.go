package main

import (
	"fmt"
	"strconv"
	"strings"
)

func RewriteHostCandidatePort(sdp string, newPort int) (string, int, error) {
	lines := strings.Split(sdp, "\r\n")
	rewritten := false
	origPort := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[4] != "127.0.0.1" || fields[7] != "host" {
			continue
		}
		port, err := strconv.Atoi(fields[5])
		if err != nil {
			return "", 0, fmt.Errorf("parse candidate port: %w", err)
		}
		fields[5] = strconv.Itoa(newPort)
		lines[i] = strings.Join(fields, " ")
		origPort = port
		rewritten = true
		break
	}
	if !rewritten {
		return "", 0, fmt.Errorf("no 127.0.0.1 host candidate found")
	}
	return strings.Join(lines, "\r\n"), origPort, nil
}
