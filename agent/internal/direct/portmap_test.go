package direct

import (
	"encoding/xml"
	"errors"
	"fmt"
	"testing"

	"github.com/huin/goupnp/soap"
)

// fakeMapper implements PortMapper without any network, letting us assert the
// contract (granted-port reporting, enumeration, ownership) in-process.
type fakeMapper struct {
	externalIP string
	granted    int // non-zero forces AddPortMapping to return this external port
	mappings   map[int]PortMapping
	listErr    error
	err        error
	deleted    []int
}

func (f *fakeMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.mappings == nil {
		f.mappings = map[int]PortMapping{}
	}
	granted := ext
	if f.granted != 0 {
		granted = f.granted
	}
	f.mappings[granted] = PortMapping{
		ExternalPort:   granted,
		InternalPort:   internal,
		InternalClient: "192.168.1.20",
		Protocol:       "TCP",
		Description:    desc,
	}
	return granted, nil
}

func (f *fakeMapper) DeletePortMapping(ext int) error {
	if f.err != nil {
		return f.err
	}
	delete(f.mappings, ext)
	f.deleted = append(f.deleted, ext)
	return nil
}

func (f *fakeMapper) ExternalIP() (string, error) { return f.externalIP, f.err }

func (f *fakeMapper) InternalIP() string { return "192.168.1.20" }

func (f *fakeMapper) ListPortMappings() ([]PortMapping, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]PortMapping, 0, len(f.mappings))
	for _, m := range f.mappings {
		out = append(out, m)
	}
	return out, nil
}

func TestPortMapperRoundTrip(t *testing.T) {
	f := &fakeMapper{externalIP: "203.0.113.7"}
	var m PortMapper = f

	granted, err := m.AddPortMapping(443, 8443, "sharebridge-test", 60)
	if err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	if granted != 443 {
		t.Fatalf("granted = %d, want 443", granted)
	}
	if f.mappings[443].InternalPort != 8443 {
		t.Fatalf("mapping not recorded: %v", f.mappings)
	}

	if ip, err := m.ExternalIP(); err != nil || ip != "203.0.113.7" {
		t.Fatalf("ExternalIP = %q, %v", ip, err)
	}
	if err := m.DeletePortMapping(443); err != nil {
		t.Fatalf("DeletePortMapping: %v", err)
	}
	if _, ok := f.mappings[443]; ok {
		t.Fatalf("mapping not removed: %v", f.mappings)
	}
}

// NAT-PMP may grant a different external port than requested; the mapper must
// report the granted port so the spike can advertise the real endpoint.
func TestPortMapper_ReportsGrantedPort(t *testing.T) {
	f := &fakeMapper{externalIP: "203.0.113.7", granted: 52000}
	var m PortMapper = f

	granted, err := m.AddPortMapping(443, 8443, "sharebridge-test", 60)
	if err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	if granted != 52000 {
		t.Fatalf("granted = %d, want 52000", granted)
	}
	if _, ok := f.mappings[52000]; !ok {
		t.Fatalf("mapping not recorded under granted port: %v", f.mappings)
	}
}

// Spec §6: never clobber an existing mapping — prefer 443 when free, fall back
// to a high dynamic-range port when 443 is occupied.
func TestChooseExternalPort_PrefersFree443(t *testing.T) {
	f := &fakeMapper{}
	got, err := ChooseExternalPort(f, 443)
	if err != nil {
		t.Fatalf("ChooseExternalPort: %v", err)
	}
	if got != 443 {
		t.Fatalf("got %d, want 443 when free", got)
	}
}

func TestChooseExternalPort_FallsBackWhen443Taken(t *testing.T) {
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, Description: "existing-nginx"},
	}}
	got, err := ChooseExternalPort(f, 443)
	if err != nil {
		t.Fatalf("ChooseExternalPort: %v", err)
	}
	if got == 443 {
		t.Fatalf("must not choose occupied 443")
	}
	if got < 49152 || got > 65535 {
		t.Fatalf("fallback %d outside dynamic range 49152-65535", got)
	}
}

// Spec §6: never delete a mapping this agent didn't create.
func TestDeleteOwnedMapping_RefusesForeign(t *testing.T) {
	want := PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "sharebridge-test"}

	tests := []struct {
		name    string
		mapping PortMapping
	}{
		{"description mismatch", PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "existing-nginx"}},
		{"internal port mismatch", PortMapping{ExternalPort: 443, InternalPort: 80, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "sharebridge-test"}},
		{"internal client mismatch", PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.99", Protocol: "TCP", Description: "sharebridge-test"}},
		{"protocol mismatch", PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "UDP", Description: "sharebridge-test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMapper{mappings: map[int]PortMapping{443: tt.mapping}}
			if err := DeleteOwnedMapping(f, 443, want); err != ErrForeignMapping {
				t.Fatalf("DeleteOwnedMapping = %v, want ErrForeignMapping", err)
			}
			if len(f.deleted) != 0 {
				t.Fatalf("foreign mapping was deleted: %v", f.deleted)
			}
			if _, ok := f.mappings[443]; !ok {
				t.Fatalf("foreign mapping disappeared: %v", f.mappings)
			}
		})
	}
}

func TestDeleteOwnedMapping_DeletesOwn(t *testing.T) {
	want := PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "sharebridge-test"}
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "sharebridge-test"},
	}}
	if err := DeleteOwnedMapping(f, 443, want); err != nil {
		t.Fatalf("DeleteOwnedMapping: %v", err)
	}
	if _, ok := f.mappings[443]; ok {
		t.Fatalf("own mapping not removed: %v", f.mappings)
	}
}

// Deleting a mapping that is not present is idempotent (spec §6): it must not
// error, and must not touch unrelated mappings.
func TestDeleteOwnedMapping_IdempotentWhenAbsent(t *testing.T) {
	want := PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "sharebridge-test"}
	f := &fakeMapper{mappings: map[int]PortMapping{
		444: {ExternalPort: 444, InternalPort: 8443, InternalClient: "192.168.1.20", Protocol: "TCP", Description: "sharebridge-test"},
	}}
	if err := DeleteOwnedMapping(f, 443, want); err != nil {
		t.Fatalf("DeleteOwnedMapping (absent) = %v, want nil", err)
	}
	if _, ok := f.mappings[444]; !ok {
		t.Fatalf("unrelated mapping disappeared: %v", f.mappings)
	}
}

// newSoapFault builds a real *soap.SOAPFaultError by unmarshalling a minimal
// SOAP <detail> fragment, so TestIsEndOfList exercises isEndOfList against the
// actual parsed goupnp fault type (not a hand-rolled stand-in).
func newSoapFault(t *testing.T, code int, desc string) *soap.SOAPFaultError {
	t.Helper()
	fragment := fmt.Sprintf(
		`<Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></Fault>`,
		code, desc,
	)
	var f soap.SOAPFaultError
	if err := xml.Unmarshal([]byte(fragment), &f); err != nil {
		t.Fatalf("xml.Unmarshal fault fragment: %v", err)
	}
	return &f
}

func TestIsEndOfList(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"errorcode 713", newSoapFault(t, 713, ""), true},
		{"errorcode 714", newSoapFault(t, 714, ""), true},
		{"description fallback SpecifiedArrayIndexInvalid", newSoapFault(t, 0, "SpecifiedArrayIndexInvalid"), true},
		{"description fallback NoSuchEntryInArray", newSoapFault(t, 0, "NoSuchEntryInArray"), true},
		{"unrelated errorcode 725", newSoapFault(t, 725, "OnlyPermanentLeasesSupported"), false},
		{"plain error containing digits 713", errors.New("mapping 47130 in use"), false},
		{"plain error with description text", errors.New("SpecifiedArrayIndexInvalid"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEndOfList(tt.err); got != tt.want {
				t.Fatalf("isEndOfList(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
