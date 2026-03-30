package opencloud

import (
	"testing"
)

func TestNew_ValidURL(t *testing.T) {
	c, err := New("https://cloud.example.com/s/AbCdEfGh", "cloud.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.token != "AbCdEfGh" {
		t.Errorf("expected token AbCdEfGh, got %s", c.token)
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://invalid-url", "cloud.example.com")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestNew_SSRF(t *testing.T) {
	_, err := New("https://evil.com/s/token", "cloud.example.com")
	if err == nil {
		t.Error("expected SSRF error for mismatched host")
	}
}

func TestParsePROPFIND(t *testing.T) {
	xml := `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/public-files/token/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/public-files/token/document.pdf</d:href>
    <d:propstat>
      <d:prop>
        <d:getcontentlength>1048576</d:getcontentlength>
        <d:getcontenttype>application/pdf</d:getcontenttype>
      </d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`

	files, err := parsePROPFIND([]byte(xml), "token")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Name != "document.pdf" {
		t.Errorf("expected document.pdf, got %s", files[0].Name)
	}
	if files[0].Size != 1048576 {
		t.Errorf("expected size 1048576, got %d", files[0].Size)
	}
}
