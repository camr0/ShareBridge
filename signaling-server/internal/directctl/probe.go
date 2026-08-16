// probe.go
package directctl

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var ssrfDeny = mustParseCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"192.31.196.0/24", "192.52.193.0/24", "192.175.48.0/24",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func isGloballyRoutable(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return false // direct path is IPv4 (UPnP/NAT-PMP)
	}
	for _, cidr := range ssrfDeny {
		if cidr.Contains(parsed) {
			return false
		}
	}
	return true
}

func (c *Controller) Probe(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
	tuple := fmt.Sprintf("%s:%d", ack.PublicIP, ack.GrantedPort)
	c.verifiedMu.Lock()
	if c.verified[apiKeyID] == tuple {
		c.verifiedMu.Unlock()
		return nil // already verified
	}
	c.verifiedMu.Unlock()

	if ack.GrantedPort < 1 || ack.GrantedPort > 65535 {
		return fmt.Errorf("invalid port %d", ack.GrantedPort)
	}
	if ack.PublicIP == "" {
		return fmt.Errorf("empty public IP")
	}
	if !isGloballyRoutable(ack.PublicIP) && !c.allowPrivate {
		return fmt.Errorf("non-routable IP: %s", ack.PublicIP)
	}

	hostPort := net.JoinHostPort(ack.PublicIP, fmt.Sprint(ack.GrantedPort))
	url := fmt.Sprintf("https://%s/s/%s/probe?nonce=%s", hostPort, code, ack.Nonce)

	client := c.probeClient
	if client == nil {
		client = &http.Client{
			Timeout: 3 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // never follow redirects
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ServerName: origin, InsecureSkipVerify: true},
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Host = origin

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK || string(body) != ack.Nonce {
		return fmt.Errorf("probe nonce mismatch (status %d)", resp.StatusCode)
	}

	c.verifiedMu.Lock()
	c.verified[apiKeyID] = tuple
	c.verifiedMu.Unlock()
	return nil
}
