package ddns

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudflare/cloudflare-go"
)

// Cloudflare manages the direct namespace's wildcard A record via the
// operator's Cloudflare API token (single zone, DNS-edit only).
type Cloudflare struct {
	api    *cloudflare.API
	zoneID string
}

func New(ctx context.Context, apiToken, zoneName string) (*Cloudflare, error) {
	api, err := cloudflare.NewWithAPIToken(apiToken, cloudflare.HTTPClient(&http.Client{Timeout: 10 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("cloudflare client: %w", err)
	}
	resp, err := api.ListZonesContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve zone %q: %w", zoneName, err)
	}
	for _, zone := range resp.Result {
		if zone.Name == zoneName {
			return &Cloudflare{api: api, zoneID: zone.ID}, nil
		}
	}
	return nil, fmt.Errorf("resolve zone %q: not found", zoneName)
}

// EnsureA idempotently points name at ip with the given TTL: it creates the
// record when missing, rewrites it only when the content or TTL drifted, and
// leaves an already-correct record untouched so repeated calls (e.g. the
// per-epoch relay wildcard provisioning) never churn the zone. proxied=false
// so the real target IP is served (no Cloudflare edge in the data path).
func (c *Cloudflare) EnsureA(ctx context.Context, name, ip string, ttl int) (string, error) {
	existing, err := c.api.DNSRecords(ctx, c.zoneID, cloudflare.DNSRecord{Type: "A", Name: name})
	if err != nil {
		return "", fmt.Errorf("list A record %q: %w", name, err)
	}
	if len(existing) == 0 {
		return c.createA(ctx, name, ip, ttl)
	}
	if existing[0].Content == ip && existing[0].TTL == ttl {
		return existing[0].ID, nil
	}
	return c.updateA(ctx, existing[0].ID, name, ip, ttl)
}

// UpsertA creates or updates the named A record to point at ip with the given
// TTL. It is idempotent at the provider through EnsureA: a record that already
// carries the requested content and TTL is left untouched.
func (c *Cloudflare) UpsertA(ctx context.Context, name, ip string, ttl int) (string, error) {
	return c.EnsureA(ctx, name, ip, ttl)
}

func (c *Cloudflare) createA(ctx context.Context, name, ip string, ttl int) (string, error) {
	proxied := false
	rec := cloudflare.DNSRecord{Type: "A", Name: name, Content: ip, TTL: ttl, Proxied: &proxied}
	resp, err := c.api.CreateDNSRecord(ctx, c.zoneID, rec)
	if err != nil {
		return "", fmt.Errorf("create A record %q: %w", name, err)
	}
	return resp.Result.ID, nil
}

func (c *Cloudflare) updateA(ctx context.Context, recordID, name, ip string, ttl int) (string, error) {
	proxied := false
	rec := cloudflare.DNSRecord{Type: "A", Name: name, Content: ip, TTL: ttl, Proxied: &proxied}
	if err := c.api.UpdateDNSRecord(ctx, c.zoneID, recordID, rec); err != nil {
		return "", fmt.Errorf("update A record %q: %w", name, err)
	}
	return recordID, nil
}

// DeleteA removes the named A record (spike cleanup).
func (c *Cloudflare) DeleteA(ctx context.Context, name string) error {
	existing, err := c.api.DNSRecords(ctx, c.zoneID, cloudflare.DNSRecord{Type: "A", Name: name})
	if err != nil {
		return err
	}
	for _, r := range existing {
		if err := c.api.DeleteDNSRecord(ctx, c.zoneID, r.ID); err != nil {
			return err
		}
	}
	return nil
}
