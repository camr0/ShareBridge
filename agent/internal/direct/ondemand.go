// agent/internal/direct/ondemand.go
package direct

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// minValidLease is the smallest lease honored. A sub-second lease would
	// collapse to int(lease.Seconds()) == 0, which some UPnP devices treat as a
	// permanent mapping; we clamp up instead.
	minValidLease = 5 * time.Second

	// renewWindow is how early, before lease expiry, the mapping is renewed
	// while any session is active, avoiding a gap where the router reclaims the
	// mapping mid-transfer.
	renewWindow = 2 * time.Second

	// closeRetryDelay is the backoff between DeletePortMapping retries.
	closeRetryDelay = 500 * time.Millisecond

	// maxCloseAttempts bounds deletion retries before the failure is escalated.
	maxCloseAttempts = 5
)

var (
	ErrPortClosed  = errors.New("direct: port is closed")
	ErrDeleteRetry = errors.New("direct: mapping deletion failed; retrying")
)

// portClock abstracts time so tests can drive the single state-loop timer
// deterministically.
type portClock interface {
	Now() time.Time
	NewTimer(d time.Duration) portTimer
}

type portTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type wallTimer struct{ t *time.Timer }

func (w wallTimer) C() <-chan time.Time { return w.t.C }
func (w wallTimer) Stop() bool          { return w.t.Stop() }

func (wallClock) NewTimer(d time.Duration) portTimer {
	return wallTimer{t: time.NewTimer(d)}
}

type portOp int

const (
	opOpenFor portOp = iota
	opBeginSession
	opActivity
	opEndSession
	opClose
	opIsOpen
	opGrantedPort
)

type portReply struct {
	open      bool
	sessionID string
	granted   int
	err       error
}

type portCommand struct {
	op        portOp
	shareID   string
	lease     time.Duration
	sessionID string
	reply     chan portReply
}

// OnDemandPort manages a public port that is closed by default and opened only
// for agent-verified, active shares. The switch is the PortMapper mapping, not
// a local listener.
//
// Concurrency model: ALL state transitions run on one goroutine (loop). A
// single timer is stopped-and-drained, then re-armed, on every transition, so a
// stale timer can never close a newly renewed mapping (the generation guard).
// The mapping stays open while any recipient session is active and is renewed
// before the lease expires; when the last session ends, an inactivity timeout
// (or the unrenewed lease) closes it. Close is idempotent, and deletion
// failures are retried with backoff then surfaced via CloseError.
type OnDemandPort struct {
	mapper      PortMapper
	extPort     int
	intPort     int
	idleTimeout time.Duration
	renewWindow time.Duration
	clock       portClock

	cmds chan portCommand
	done chan struct{}

	closeMu  sync.RWMutex
	closeErr error
}

func NewOnDemandPort(mapper PortMapper, extPort int) *OnDemandPort {
	return NewOnDemandPortOpts(mapper, extPort, extPort, 5*time.Minute)
}

func NewOnDemandPortOpts(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration) *OnDemandPort {
	p := &OnDemandPort{
		mapper:      mapper,
		extPort:     extPort,
		intPort:     intPort,
		idleTimeout: idleTimeout,
		renewWindow: renewWindow,
		clock:       wallClock{},
		cmds:        make(chan portCommand),
		done:        make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *OnDemandPort) desc() string { return fmt.Sprintf("sharebridge-direct-%d", p.extPort) }

func (p *OnDemandPort) send(c portCommand) portReply {
	select {
	case p.cmds <- c:
		return <-c.reply
	case <-p.done:
		return portReply{open: false, err: ErrPortClosed}
	}
}

// Open reports whether the port is currently mapped.
func (p *OnDemandPort) Open() bool {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opIsOpen, reply: ch}).open
}

// OpenFor maps (or re-maps/renews) the port for shareID with the given lease.
// Leases below minValidLease are clamped up. Only shares the agent has
// independently registered and source-verified may be passed here; the signal
// handler enforces that (see opensignal.go), not this type.
func (p *OnDemandPort) OpenFor(shareID string, lease time.Duration) error {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opOpenFor, shareID: shareID, lease: lease, reply: ch}).err
}

// BeginSession records a new recipient session for shareID and returns an
// opaque session token. It fails while the port is closed.
func (p *OnDemandPort) BeginSession(shareID string) (string, error) {
	ch := make(chan portReply, 1)
	r := p.send(portCommand{op: opBeginSession, shareID: shareID, reply: ch})
	return r.sessionID, r.err
}

// Activity records that sessionID is still active, postponing the idle close.
func (p *OnDemandPort) Activity(sessionID string) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opActivity, sessionID: sessionID, reply: ch})
}

// EndSession ends sessionID. When the last session ends, the port stays mapped
// only until the inactivity timeout (or the unrenewed lease), then closes.
func (p *OnDemandPort) EndSession(sessionID string) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opEndSession, sessionID: sessionID, reply: ch})
}

// Close removes the mapping immediately and idempotently (lockdown or expiry).
// On deletion failure it returns ErrDeleteRetry and retries with backoff; after
// maxCloseAttempts the failure is escalated and stays visible via CloseError.
func (p *OnDemandPort) Close() error {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opClose, reply: ch}).err
}

// GrantedPort returns the external port the router actually granted (NAT-PMP
// may remap), for endpoint reporting. Zero until the port is mapped.
func (p *OnDemandPort) GrantedPort() int {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opGrantedPort, reply: ch}).granted
}

// CloseError returns the last mapping-deletion failure (nil once deletion
// succeeds), for alerting/escalation.
func (p *OnDemandPort) CloseError() error {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	return p.closeErr
}

func (p *OnDemandPort) setCloseErr(err error) {
	p.closeMu.Lock()
	p.closeErr = err
	p.closeMu.Unlock()
}

func (p *OnDemandPort) loop() {
	defer close(p.done)

	var (
		open        bool
		closing     bool
		renewFailed bool
		lease       time.Duration
		grantedPort int       // external port the router actually granted (NAT-PMP may remap)
		deadline    time.Time // lease expiry if never renewed
		renewAt     time.Time // when to renew (while sessions are active)
		idleAt      time.Time // inactivity close (zero until a session exists)
		sessions    = map[string]time.Time{}
		seq         uint64
		timer       portTimer
		timerC      <-chan time.Time
		closeFail   int
	)

	clearTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C():
				default:
				}
			}
		}
		timer, timerC = nil, nil
	}
	arm := func(d time.Duration) {
		clearTimer()
		timer = p.clock.NewTimer(d)
		timerC = timer.C()
	}
	until := func(now, next time.Time) time.Duration {
		d := next.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	rearm := func() {
		clearTimer()
		now := p.clock.Now()
		switch {
		case closing:
			arm(closeRetryDelay)
		case open && len(sessions) > 0:
			next := renewAt
			if idleAt.Before(next) {
				next = idleAt
			}
			arm(until(now, next))
		case open:
			next := deadline
			if !idleAt.IsZero() && idleAt.Before(next) {
				next = idleAt
			}
			arm(until(now, next))
		}
	}
	tryDelete := func() bool {
		port := p.extPort
		if grantedPort != 0 {
			port = grantedPort
		}
		// DeleteOwnedMapping refuses to delete a mapping this agent didn't
		// create (spec §6), rather than deleting blindly.
		if err := DeleteOwnedMapping(p.mapper, port); err != nil {
			closeFail++
			p.setCloseErr(err)
			return false
		}
		closing = false
		closeFail = 0
		grantedPort = 0
		p.setCloseErr(nil)
		return true
	}
	startClose := func() {
		open = false
		closing = true
		closeFail = 0
		sessions = map[string]time.Time{} // drop stale sessions so a reopen can't renew from them
		idleAt = time.Time{}
		rearm() // first DeletePortMapping happens on the next tick
	}

	for {
		select {
		case c := <-p.cmds:
			switch c.op {
			case opOpenFor:
				l := c.lease
				if l < minValidLease {
					l = minValidLease
				}
				port := p.extPort
				if grantedPort != 0 {
					port = grantedPort
				}
				granted, err := p.mapper.AddPortMapping(port, p.intPort, p.desc(), int(l.Seconds()))
				if err != nil {
					// Leave closing intact on failure: the old mapping may still
					// exist on the router, so the armed delete-retry timer must
					// keep firing rather than being silently abandoned.
					c.reply <- portReply{err: err}
					continue
				}
				if closing {
					closing = false
					closeFail = 0
				}
				grantedPort = granted
				renewFailed = false
				open = true
				now := p.clock.Now()
				lease = l
				deadline = now.Add(l)
				renewAt = deadline.Add(-p.renewWindow)
				if !renewAt.After(now) {
					renewAt = now.Add(p.renewWindow)
				}
				rearm()
				c.reply <- portReply{err: nil}

			case opBeginSession:
				if !open {
					c.reply <- portReply{err: ErrPortClosed}
					continue
				}
				seq++
				id := fmt.Sprintf("sess-%d", seq)
				sessions[id] = p.clock.Now()
				idleAt = sessions[id].Add(p.idleTimeout)
				rearm()
				c.reply <- portReply{sessionID: id}

			case opActivity:
				if _, ok := sessions[c.sessionID]; ok {
					now := p.clock.Now()
					sessions[c.sessionID] = now
					idleAt = now.Add(p.idleTimeout)
					rearm()
				}
				c.reply <- portReply{}

			case opEndSession:
				if _, ok := sessions[c.sessionID]; ok {
					delete(sessions, c.sessionID)
					if len(sessions) == 0 {
						idleAt = p.clock.Now().Add(p.idleTimeout)
					}
					rearm()
				}
				c.reply <- portReply{}

			case opClose:
				switch {
				case open:
					open = false
					closing = true
					closeFail = 0
					sessions = map[string]time.Time{}
					idleAt = time.Time{}
					if tryDelete() {
						c.reply <- portReply{}
					} else {
						rearm()
						c.reply <- portReply{err: ErrDeleteRetry}
					}
				case closing:
					c.reply <- portReply{err: ErrDeleteRetry}
				default:
					c.reply <- portReply{} // idempotent: already closed
				}

			case opIsOpen:
				c.reply <- portReply{open: open}

			case opGrantedPort:
				c.reply <- portReply{granted: grantedPort}
			}

		case <-timerC:
			// The timer can only be the one armed for the current state:
			// clearTimer stops+drains before every re-arm, so a stale fire is
			// impossible. Each branch re-derives from current state.
			switch {
			case closing:
				if tryDelete() {
					rearm()
				} else if closeFail >= maxCloseAttempts {
					closing = false
					rearm() // mapping remains; failure surfaced via CloseError
				} else {
					rearm()
				}
			case open && len(sessions) > 0:
				now := p.clock.Now()
				switch {
				case !now.Before(idleAt):
					startClose()
				case renewFailed:
					// a prior renewal failed; wait for lease expiry, then close
					if !now.Before(deadline) {
						startClose()
					} else {
						rearm()
					}
				case !now.Before(renewAt):
					port := p.extPort
					if grantedPort != 0 {
						port = grantedPort
					}
					granted, err := p.mapper.AddPortMapping(port, p.intPort, p.desc(), int(lease.Seconds()))
					if err != nil {
						renewFailed = true
						renewAt = deadline // stop renewing; close at lease expiry
						rearm()
					} else {
						grantedPort = granted
						deadline = now.Add(lease)
						renewAt = deadline.Add(-p.renewWindow)
						rearm()
					}
				default:
					rearm()
				}
			case open:
				now := p.clock.Now()
				if !idleAt.IsZero() && !now.Before(idleAt) {
					startClose()
				} else if !now.Before(deadline) {
					startClose()
				} else {
					rearm()
				}
			}
		}
	}
}
