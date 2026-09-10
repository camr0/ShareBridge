package direct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/soap"
	"github.com/jackpal/gateway"
	"github.com/jackpal/go-nat-pmp"
)

// RouterIOTimeout bounds every router round trip the agent makes (UPnP SOAP
// and NAT-PMP). A LAN-local round trip answers in single-digit milliseconds,
// so this is roughly three orders of magnitude of headroom for a slow or
// heavily loaded router, while keeping the worst-case lockdown mapping-close
// lever (one bounded enumeration plus one bounded delete = 2×RouterIOTimeout
// = 6s) comfortably inside defaultLockdownLeverTimeout (10s). The bound is a
// router I/O bound — it applies to each device call — not merely a bound on
// the lockdown lever aggregation, so a hidden router cannot hold the
// OnDemandPort state loop (and therefore OpenFor) open past it.
const RouterIOTimeout = 3 * time.Second

// DescriptionPrefix marks mappings this agent created, so it never deletes or
// overwrites a mapping owned by another service (spec §6).
const DescriptionPrefix = "sharebridge"

var (
	// ErrListingUnsupported is returned by ListPortMappings for devices
	// (NAT-PMP) that cannot enumerate their mappings.
	ErrListingUnsupported = errors.New("port mapping listing not supported")
	// ErrForeignMapping is returned when refusing to delete a mapping this
	// agent did not create.
	ErrForeignMapping = errors.New("refusing to delete a port mapping this agent did not create")
)

// PortMapping is one existing mapping on the router.
type PortMapping struct {
	ExternalPort   int
	InternalPort   int
	InternalClient string
	Protocol       string
	Description    string
}

// PortMapper maps an external port on the router to the agent's internal
// address. AddPortMapping returns the external port actually granted, which
// may differ from the requested one (NAT-PMP may remap).
type PortMapper interface {
	AddPortMapping(externalPort, internalPort int, description string, leaseSeconds int) (int, error)
	DeletePortMapping(externalPort int) error
	ExternalIP() (string, error)
	ListPortMappings() ([]PortMapping, error)
	InternalIP() string
}

// upnpConnection abstracts WANIPConnection1 and WANPPPConnection1, which have
// identical method sets but distinct generated types. Only the Ctx variants
// are used: the legacy ones hardcode context.Background(), which would make
// every call unbounded against a wedged router.
type upnpConnection interface {
	AddPortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string, NewInternalPort uint16, NewInternalClient string, NewEnabled bool, NewPortMappingDescription string, NewLeaseDuration uint32) error
	DeletePortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	GetGenericPortMappingEntryCtx(ctx context.Context, NewPortMappingIndex uint16) (NewRemoteHost string, NewExternalPort uint16, NewProtocol string, NewInternalPort uint16, NewInternalClient string, NewEnabled bool, NewPortMappingDescription string, NewLeaseDuration uint32, err error)
}

// UPnPMapper maps ports using UPnP IGD (WANIPConnection1 or WANPPPConnection1).
type UPnPMapper struct {
	client     upnpConnection
	internalIP string
	timeout    time.Duration
}

func NewUPnPMapper(ctx context.Context) (*UPnPMapper, error) {
	client, err := discoverUPnPClient(ctx)
	if err != nil {
		return nil, err
	}
	internal, err := lanAddressTowardsGateway()
	if err != nil {
		return nil, fmt.Errorf("resolve LAN address toward gateway: %w", err)
	}
	return &UPnPMapper{client: client, internalIP: internal, timeout: RouterIOTimeout}, nil
}

// routerContext bounds one router round trip. goupnp's generated legacy
// methods pass context.Background() (and its SOAP client sets no HTTP
// timeout), so a wedged router — one that accepts the connection and never
// answers — would otherwise block the caller forever.
func (m *UPnPMapper) routerContext() (context.Context, context.CancelFunc) {
	timeout := m.timeout
	if timeout <= 0 {
		timeout = RouterIOTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

// discoverUPnPClient tries WANIP then WANPPP (some routers expose only one).
func discoverUPnPClient(ctx context.Context) (upnpConnection, error) {
	if clients, _, err := internetgateway1.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return clients[0], nil
	}
	if clients, _, err := internetgateway1.NewWANPPPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return clients[0], nil
	}
	return nil, fmt.Errorf("no UPnP IGD gateway (WANIPConnection or WANPPPConnection) found")
}

func (m *UPnPMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	// The internal client is this host's real LAN address (not 0.0.0.0),
	// resolved toward the gateway so the router forwards to the right host.
	ctx, cancel := m.routerContext()
	defer cancel()
	if err := m.client.AddPortMappingCtx(ctx, "", uint16(ext), "TCP", uint16(internal), m.internalIP, true, desc, uint32(lease)); err != nil {
		return 0, err
	}
	return ext, nil
}

func (m *UPnPMapper) DeletePortMapping(ext int) error {
	ctx, cancel := m.routerContext()
	defer cancel()
	return m.client.DeletePortMappingCtx(ctx, "", uint16(ext), "TCP")
}

func (m *UPnPMapper) ExternalIP() (string, error) {
	ctx, cancel := m.routerContext()
	defer cancel()
	return m.client.GetExternalIPAddressCtx(ctx)
}

func (m *UPnPMapper) InternalIP() string { return m.internalIP }

// ListPortMappings enumerates the router's mappings under ONE deadline: the
// enumeration is up to 256 round trips, so bounding each entry individually
// would still let a wedged router hold the caller for minutes. The shared
// context aborts the whole walk at RouterIOTimeout.
func (m *UPnPMapper) ListPortMappings() ([]PortMapping, error) {
	ctx, cancel := m.routerContext()
	defer cancel()
	var out []PortMapping
	for i := 0; i < 256; i++ {
		_, ext, proto, internal, client, _, desc, _, err := m.client.GetGenericPortMappingEntryCtx(ctx, uint16(i))
		if err != nil {
			if isEndOfList(err) {
				return out, nil // end-of-list reached cleanly
			}
			// A real enumeration error must NOT be treated as an empty list —
			// otherwise an occupied 443 could look free (fail closed). The
			// same applies to the shared deadline: a truncated walk must never
			// read as "port 443 is free".
			return nil, fmt.Errorf("enumerate port mapping %d: %w", i, err)
		}
		out = append(out, PortMapping{
			ExternalPort:   int(ext),
			InternalPort:   int(internal),
			InternalClient: client,
			Protocol:       proto,
			Description:    desc,
		})
	}
	// Reached the 256-entry cap without an end-of-list fault — the list is
	// incomplete, so fail closed rather than returning a partial view.
	return nil, fmt.Errorf("port mapping enumeration exceeded 256 entries without end-of-list")
}

// isEndOfList reports whether err is the UPnP IGD "end of list" fault.
// It matches the parsed SOAP fault structurally — error code 713
// (SpecifiedArrayIndexInvalid) or 714 (NoSuchEntryInArray), with the
// errorDescription as a fallback for routers that omit the numeric code.
// Any other error (including a generic SOAP invalid-argument fault, or an
// error whose text merely contains the digits "713") is a real enumeration
// failure and must NOT be mistaken for end-of-list.
func isEndOfList(err error) bool {
	var soapErr *soap.SOAPFaultError
	if !errors.As(err, &soapErr) {
		return false
	}
	switch soapErr.Detail.UPnPError.Errorcode {
	case 713, 714:
		return true
	}
	switch soapErr.Detail.UPnPError.ErrorDescription {
	case "SpecifiedArrayIndexInvalid", "NoSuchEntryInArray":
		return true
	}
	return false
}

// NATPMPMapper maps ports using NAT-PMP (Apple/older routers).
type NATPMPMapper struct {
	client *natpmp.Client

	mu       sync.Mutex
	internal int // internal port of the active mapping (0 = none)
	external int // granted external port of the active mapping
}

func NewNATPMPMapper(gatewayIP net.IP) *NATPMPMapper {
	return NewNATPMPMapperWithTimeout(gatewayIP, RouterIOTimeout)
}

// NewNATPMPMapperWithTimeout builds a NAT-PMP mapper whose whole RPC retry
// budget is bounded. go-nat-pmp's default is ~128 seconds, far beyond the
// lockdown lever bound; the caller's bound keeps a wedged NAT-PMP gateway
// from holding the OnDemandPort state loop open.
func NewNATPMPMapperWithTimeout(gatewayIP net.IP, timeout time.Duration) *NATPMPMapper {
	if timeout <= 0 {
		timeout = RouterIOTimeout
	}
	return &NATPMPMapper{client: natpmp.NewClientWithTimeout(gatewayIP, timeout)}
}

func (m *NATPMPMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	// The returned MappedExternalPort is the router's chosen port; it may
	// differ from the requested ext, so it must be retained and returned.
	result, err := m.client.AddPortMapping("tcp", internal, ext, lease)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.internal, m.external = internal, int(result.MappedExternalPort)
	m.mu.Unlock()
	return int(result.MappedExternalPort), nil
}

func (m *NATPMPMapper) DeletePortMapping(ext int) error {
	// NAT-PMP has no separate delete; re-issuing the mapping with a zero lease
	// removes it. `ext` is used only if no mapping was recorded by a prior
	// AddPortMapping.
	m.mu.Lock()
	internal, external := m.internal, m.external
	m.mu.Unlock()
	if external == 0 {
		external = ext
	}
	_, err := m.client.AddPortMapping("tcp", internal, external, 0)
	return err
}

func (m *NATPMPMapper) ExternalIP() (string, error) {
	resp, err := m.client.GetExternalAddress()
	if err != nil {
		return "", err
	}
	ip := resp.ExternalIPAddress
	return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3]), nil
}

// InternalIP is empty for NAT-PMP: the protocol has no notion of an internal
// client address (the router forwards to the requesting host by construction).
func (m *NATPMPMapper) InternalIP() string { return "" }

func (m *NATPMPMapper) ListPortMappings() ([]PortMapping, error) {
	return nil, ErrListingUnsupported
}

// MapperForRouter returns the first available mapper, trying UPnP then NAT-PMP.
// The NAT-PMP gateway address is discovered via github.com/jackpal/gateway,
// never hardcoded.
func MapperForRouter(ctx context.Context) (PortMapper, error) {
	if m, err := NewUPnPMapper(ctx); err == nil {
		return m, nil
	}
	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return nil, fmt.Errorf("no UPnP IGD or NAT-PMP gateway found: %w", err)
	}
	return NewNATPMPMapper(gw), nil
}

// ChooseExternalPort picks the external port for a new mapping honoring spec
// §6: prefer `preferred` (443), but never clobber an existing mapping — fall
// back to the first free dynamic-range port when `preferred` is occupied.
// When listing is unsupported (NAT-PMP), 443 is treated as unsafe (assumed
// occupied) and any other preferred port is used as-is.
func ChooseExternalPort(mapper PortMapper, preferred int) (int, error) {
	mappings, err := mapper.ListPortMappings()
	switch {
	case errors.Is(err, ErrListingUnsupported):
		if preferred == 443 {
			return firstFreePort(nil), nil
		}
		return preferred, nil
	case err != nil:
		return 0, fmt.Errorf("list port mappings: %w", err)
	}
	if occupied(mappings, preferred) {
		p := firstFreePort(mappings)
		if p == 0 {
			return 0, fmt.Errorf("no free port in dynamic range 49152-65535")
		}
		return p, nil
	}
	return preferred, nil
}

// DeleteOwnedMapping removes the mapping at externalPort only if it EXACTLY
// matches want (description, internal port, internal client, protocol). A
// mapping that differs in any of those fields belongs to another service or
// agent, so deletion is refused with ErrForeignMapping rather than clobbering
// it. If there is no mapping at externalPort, the call is a no-op (idempotent).
// Mappers without listing support (NAT-PMP) are deleted best-effort: their
// mappings expire on their own and we tracked the granted port ourselves.
func DeleteOwnedMapping(mapper PortMapper, externalPort int, want PortMapping) error {
	mappings, err := mapper.ListPortMappings()
	switch {
	case errors.Is(err, ErrListingUnsupported):
		return mapper.DeletePortMapping(externalPort)
	case err != nil:
		return fmt.Errorf("list port mappings: %w", err)
	}
	for _, m := range mappings {
		if m.ExternalPort != externalPort {
			continue
		}
		if m.Description != want.Description || m.InternalPort != want.InternalPort ||
			m.InternalClient != want.InternalClient || m.Protocol != want.Protocol {
			return ErrForeignMapping
		}
		return mapper.DeletePortMapping(externalPort)
	}
	return nil // no mapping at that port (idempotent)
}

// lanAddressTowardsGateway returns this host's IP on the interface that routes
// toward the default gateway — the address the router must forward the mapped
// port to (never 0.0.0.0, which some routers reject).
func lanAddressTowardsGateway() (string, error) {
	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return "", err
	}
	return localAddrTowards(gw)
}

func localAddrTowards(dst net.IP) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(dst.String(), "9"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	return host, err
}

func occupied(mappings []PortMapping, port int) bool {
	for _, m := range mappings {
		if m.ExternalPort == port {
			return true
		}
	}
	return false
}

// firstFreePort returns the first port in the IANA dynamic range (49152-65535)
// not present in mappings. A deterministic scan (rather than a random pick)
// keeps the spike testable; a random high port is equally valid in production.
func firstFreePort(mappings []PortMapping) int {
	for p := 49152; p <= 65535; p++ {
		if !occupied(mappings, p) {
			return p
		}
	}
	return 0
}
