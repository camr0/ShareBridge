package quota

import (
	"log"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
)

// Accumulator receives per-circuit byte counts from relay.OnCircuitClosed and
// flushes them to PocketBase on a regular interval.
type Accumulator struct {
	app  core.App
	cfg  *config.Config
	mu   sync.Mutex
	buf  map[string]int64 // apiKeyID → bytes accrued since last flush
	stop chan struct{}
}

func NewAccumulator(app core.App, cfg *config.Config) *Accumulator {
	return &Accumulator{app: app, cfg: cfg, buf: make(map[string]int64), stop: make(chan struct{})}
}

// Record adds bytes to the per-apiKeyID bucket. Called from relay.OnCircuitClosed.
// Both directions are summed — every relayed byte counts toward quota.
func (a *Accumulator) Record(apiKeyID string, bytesIn, bytesOut int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf[apiKeyID] += bytesIn + bytesOut
}

func (a *Accumulator) Start() { go a.run() }

func (a *Accumulator) Stop() { close(a.stop) }

func (a *Accumulator) run() {
	t := time.NewTicker(a.cfg.QuotaCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.flush()
		case <-a.stop:
			a.flush()
			return
		}
	}
}

func (a *Accumulator) flush() {
	a.mu.Lock()
	snapshot := a.buf
	a.buf = make(map[string]int64)
	a.mu.Unlock()

	for apiKeyID, bytes := range snapshot {
		if bytes == 0 {
			continue
		}
		apiKey, err := a.app.FindRecordById("api_keys", apiKeyID)
		if err != nil {
			log.Printf("quota flush: api_keys %s: %v", apiKeyID, err)
			continue
		}
		accountID := apiKey.GetString("account_id")
		acct, err := a.app.FindRecordById("users", accountID)
		if err != nil {
			log.Printf("quota flush: users %s: %v", accountID, err)
			continue
		}
		currentGB := acct.GetFloat("current_period_usage_gb")
		acct.Set("current_period_usage_gb", currentGB+float64(bytes)/(1024*1024*1024))
		if err := a.app.Save(acct); err != nil {
			log.Printf("quota flush: save account %s: %v", accountID, err)
		}
	}
}