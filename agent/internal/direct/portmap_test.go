package direct

import "testing"

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
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, Description: "existing-nginx"},
	}}
	if err := DeleteOwnedMapping(f, 443); err != ErrForeignMapping {
		t.Fatalf("DeleteOwnedMapping = %v, want ErrForeignMapping", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("foreign mapping was deleted: %v", f.deleted)
	}
	if _, ok := f.mappings[443]; !ok {
		t.Fatalf("foreign mapping disappeared: %v", f.mappings)
	}
}

func TestDeleteOwnedMapping_DeletesOwn(t *testing.T) {
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, Description: "sharebridge-test"},
	}}
	if err := DeleteOwnedMapping(f, 443); err != nil {
		t.Fatalf("DeleteOwnedMapping: %v", err)
	}
	if _, ok := f.mappings[443]; ok {
		t.Fatalf("own mapping not removed: %v", f.mappings)
	}
}
