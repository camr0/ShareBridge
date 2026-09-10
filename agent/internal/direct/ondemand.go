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

// PortState is the lifecycle state of the on-demand port mapping.
type PortState int

const (
	StateClosed PortState = iota
	StateOpen
	StateClosing
	StateCloseFailed
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
	opBeginHold
	opEndHold
	opClose
	opCloseIf
	opIsOpen
	opGrantedPort
	opState
	opSetCallback
)

type portReply struct {
	open      bool
	sessionID string
	granted   int
	token     uint64
	err       error
	wasOpen   bool
	state     PortState
}

type portCommand struct {
	op        portOp
	shareID   string
	lease     time.Duration
	sessionID string
	token     uint64
	callback  func(old, new PortState, grantedPort int)
	reply     chan portReply

	// stillCurrent is the optional generation fence of opCloseIf (see CloseIf).
	stillCurrent func() bool
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
//
// Ownership is per-agent: the mapping's description is "<descPrefix>-<extPort>"
// and its internal client is intClient, so DeleteOwnedMapping only ever removes
// a mapping this exact agent created (see portmap.go).
type OnDemandPort struct {
	mapper      PortMapper
	extPort     int
	intPort     int
	idleTimeout time.Duration
	renewWindow time.Duration
	clock       portClock

	descPrefix string
	intClient  string

	// state and cb are only read/written on the loop goroutine (via setState
	// and the opState/opSetCallback commands), so they need no separate lock.
	state PortState
	cb    func(old, new PortState, grantedPort int)

	cmds chan portCommand
	done chan struct{}

	closeMu  sync.RWMutex
	closeErr error
}

func NewOnDemandPort(mapper PortMapper, extPort int) *OnDemandPort {
	return NewOnDemandPortOpts(mapper, extPort, extPort, 5*time.Minute)
}

func NewOnDemandPortOpts(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration) *OnDemandPort {
	return NewOnDemandPortOwned(mapper, extPort, intPort, idleTimeout, DescriptionPrefix, mapper.InternalIP())
}

func NewOnDemandPortOwned(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration, descPrefix, intClient string) *OnDemandPort {
	p := &OnDemandPort{
		mapper:      mapper,
		extPort:     extPort,
		intPort:     intPort,
		idleTimeout: idleTimeout,
		renewWindow: renewWindow,
		clock:       wallClock{},
		descPrefix:  descPrefix,
		intClient:   intClient,
		cmds:        make(chan portCommand),
		done:        make(chan struct{}),
		state:       StateClosed,
	}
	go p.loop()
	return p
}

func (p *OnDemandPort) desc() string { return fmt.Sprintf("%s-%d", p.descPrefix, p.extPort) }

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

// State returns the current lifecycle state of the port mapping.
func (p *OnDemandPort) State() PortState {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opState, reply: ch}).state
}

// SetTransitionCallback registers a callback invoked synchronously on the
// state loop whenever the port's state changes. It receives the old state, the
// new state, and the granted external port in effect (zero once closed). A nil
// callback clears any previously registered one.
func (p *OnDemandPort) SetTransitionCallback(cb func(old, new PortState, grantedPort int)) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opSetCallback, callback: cb, reply: ch})
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

// Begin records an in-flight hold — a long streaming response in progress — so
// the idle close is paused while at least one hold is active. It returns an
// opaque hold token bound to the current open epoch; End(token) ignores a token
// from a prior epoch (e.g. a deferred release that runs after a force-close and
// reopen). It is a no-op, returning the zero token, while the port is closed
// (there is nothing to hold open).
func (p *OnDemandPort) Begin() uint64 {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opBeginHold, reply: ch}).token
}

// End releases the hold identified by token, as returned by Begin. A stale
// token from a prior open epoch — a deferred release that runs after the port
// was force-closed and reopened for a different stream — is ignored so it can
// never decrement the new epoch's in-flight count. The zero token is likewise a
// no-op. When the last hold of the current epoch is released after the idle
// deadline has already passed, the port closes immediately.
func (p *OnDemandPort) End(token uint64) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opEndHold, token: token, reply: ch})
}

// Close removes the mapping immediately and idempotently (lockdown or expiry).
// On deletion failure it returns ErrDeleteRetry and retries with backoff; after
// maxCloseAttempts the failure is escalated and stays visible via CloseError.
func (p *OnDemandPort) Close() error {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opClose, reply: ch}).err
}

// CloseIf removes the mapping exactly like Close, but only while stillCurrent
// reports that the caller's generation is still current. stillCurrent is
// evaluated ON THE STATE LOOP immediately before the close decision, so it is
// atomic with respect to a concurrent OpenFor — the single-threaded loop
// serializes the two. That makes a superseded caller (a §13.4 lockdown lever
// that overran the aggregation bound past an Unlock) provably unable to close
// a mapping the newer generation opened, without the caller holding any lock
// across the router I/O. A nil stillCurrent is equivalent to Close.
func (p *OnDemandPort) CloseIf(stillCurrent func() bool) error {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opCloseIf, stillCurrent: stillCurrent, reply: ch}).err
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

// setState runs on the loop goroutine. It fires the transition callback (if
// any) only on an actual state change, then records the new state.
func (p *OnDemandPort) setState(next PortState, grantedPort int) {
	if p.cb != nil && p.state != next {
		p.cb(p.state, next, grantedPort)
	}
	p.state = next
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
		inFlight    int    // active streaming holds; pause the idle close while > 0
		epoch       uint64 // bumped on each open; hold tokens bind to this epoch
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
			if inFlight == 0 && !idleAt.IsZero() && idleAt.Before(next) {
				next = idleAt
			}
			arm(until(now, next))
		case open:
			next := deadline
			if inFlight == 0 && !idleAt.IsZero() && idleAt.Before(next) {
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
		// create (spec §6), rather than deleting blindly. The want identity is
		// exact: same description, internal port, internal client, protocol.
		want := PortMapping{
			ExternalPort:   port,
			InternalPort:   p.intPort,
			InternalClient: p.intClient,
			Protocol:       "TCP",
			Description:    p.desc(),
		}
		if err := DeleteOwnedMapping(p.mapper, port, want); err != nil {
			closeFail++
			p.setCloseErr(err)
			p.setState(StateClosing, port)
			return false
		}
		closing = false
		closeFail = 0
		grantedPort = 0
		p.setCloseErr(nil)
		p.setState(StateClosed, 0)
		return true
	}
	startClose := func() {
		open = false
		closing = true
		closeFail = 0
		sessions = map[string]time.Time{} // drop stale sessions so a reopen can't renew from them
		idleAt = time.Time{}
		inFlight = 0 // drop stale holds so a reopen can't stay held open forever
		p.setState(StateClosing, grantedPort)
		rearm() // first DeletePortMapping happens on the next tick
	}
	// closeNow is the shared body of opClose/opCloseIf. Its state mutation is
	// the loop's own, so a fence evaluated by the caller immediately before
	// this call is atomic with respect to any queued opOpenFor.
	closeNow := func() portReply {
		switch {
		case open:
			open = false
			closing = true
			closeFail = 0
			sessions = map[string]time.Time{}
			idleAt = time.Time{}
			inFlight = 0
			p.setState(StateClosing, grantedPort)
			if tryDelete() {
				return portReply{}
			}
			rearm()
			return portReply{err: ErrDeleteRetry}
		case closing:
			return portReply{err: ErrDeleteRetry}
		default:
			return portReply{} // idempotent: already closed
		}
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
				now := p.clock.Now()

				if !open {
					// Cold open. If a mapping from a prior open lingers
					// (closing or close-failed), delete it before re-selecting
					// — never two mappings on the router at once.
					if grantedPort != 0 {
						if !tryDelete() {
							c.reply <- portReply{err: ErrDeleteRetry}
							continue
						}
					}
					requested, err := ChooseExternalPort(p.mapper, p.extPort)
					if err != nil {
						c.reply <- portReply{err: err}
						continue
					}
					p.extPort = requested
					granted, err := p.mapper.AddPortMapping(p.extPort, p.intPort, p.desc(), int(l.Seconds()))
					if err != nil {
						c.reply <- portReply{err: err}
						continue
					}
					grantedPort = granted
					open = true
					epoch++ // new open epoch: hold tokens from a prior open can no longer match
					closing = false
					closeFail = 0
					renewFailed = false
					lease = l
					deadline = now.Add(l)
					renewAt = deadline.Add(-p.renewWindow)
					if !renewAt.After(now) {
						renewAt = now.Add(p.renewWindow)
					}
					idleAt = time.Time{}
					p.setState(StateOpen, grantedPort)
					rearm()
					c.reply <- portReply{err: nil, open: true, granted: grantedPort, wasOpen: false}
					continue
				}

				// Fast path: already open. A fresh signal means a fresh
				// recipient, so clear the inactivity-close deadline — but only
				// while no session is active. With active sessions idleAt is
				// owned by session activity; zeroing it would discard the
				// active sessions' idle deadline (a zero idleAt would make
				// rearm arm an immediate close).
				wasOpen := true
				if len(sessions) == 0 {
					idleAt = time.Time{}
				}
				if now.Add(l).After(deadline) {
					granted, err := p.mapper.AddPortMapping(grantedPort, p.intPort, p.desc(), int(l.Seconds()))
					if err != nil {
						renewFailed = true
						renewAt = deadline
						c.reply <- portReply{err: err}
						continue
					}
					grantedPort = granted
					renewFailed = false
					deadline = now.Add(l)
					renewAt = deadline.Add(-p.renewWindow)
				}
				rearm()
				c.reply <- portReply{err: nil, open: true, granted: grantedPort, wasOpen: wasOpen}

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

			case opBeginHold:
				if open {
					inFlight++
					rearm()
					c.reply <- portReply{token: epoch}
				} else {
					c.reply <- portReply{}
				}

			case opEndHold:
				// A stale token from a prior open epoch must not touch the
				// current epoch's in-flight count: after a force-close and
				// reopen the epoch has advanced, so the old stream's deferred
				// release is ignored here.
				if c.token == 0 || c.token != epoch {
					c.reply <- portReply{}
					continue
				}
				released := inFlight > 0
				if released {
					inFlight--
				}
				if open && released && inFlight == 0 {
					now := p.clock.Now()
					if !idleAt.IsZero() && !now.Before(idleAt) {
						startClose()
					} else {
						rearm()
					}
				}
				c.reply <- portReply{}

			case opClose:
				c.reply <- closeNow()

			case opCloseIf:
				// Close only while the caller's generation still owns this port.
				// A superseded caller must not close a mapping that a
				// post-unlock OpenFor opened.
				if c.stillCurrent != nil && !c.stillCurrent() {
					c.reply <- portReply{}
					continue
				}
				c.reply <- closeNow()

			case opIsOpen:
				c.reply <- portReply{open: open}

			case opGrantedPort:
				c.reply <- portReply{granted: grantedPort}

			case opState:
				c.reply <- portReply{state: p.state}

			case opSetCallback:
				p.cb = c.callback
				c.reply <- portReply{}
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
					p.setState(StateCloseFailed, grantedPort)
					rearm() // mapping remains; failure surfaced via CloseError
				} else {
					rearm()
				}
			case open && len(sessions) > 0:
				now := p.clock.Now()
				switch {
				case inFlight == 0 && !idleAt.IsZero() && !now.Before(idleAt):
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
						renewFailed = false
						deadline = now.Add(lease)
						renewAt = deadline.Add(-p.renewWindow)
						rearm()
					}
				default:
					rearm()
				}
			case open:
				now := p.clock.Now()
				if inFlight == 0 && !idleAt.IsZero() && !now.Before(idleAt) {
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
