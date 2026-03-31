package opencloud

import (
	"testing"
)

func TestNew_ValidURL(t *testing.T) {
	c, err := New("https://cloud.example.com/s/AbCdEfGh", "cloud.example.com", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.token != "AbCdEfGh" {
		t.Errorf("expected token AbCdEfGh, got %s", c.token)
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://invalid-url", "cloud.example.com", "")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestNew_SSRF(t *testing.T) {
	_, err := New("https://evil.com/s/token", "cloud.example.com", "")
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

func TestParsePROPFIND_WithChecksums(t *testing.T) {
	xmlData := `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/public-files/token/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/public-files/token/video.mp4</d:href>
    <d:propstat>
      <d:prop>
        <d:getcontentlength>1048576</d:getcontentlength>
        <d:getcontenttype>video/mp4</d:getcontenttype>
        <oc:checksums>
          <oc:checksum>SHA1:a7e0206e573edbef0c4d8107a151271fbeccf2fe MD5:f77a740d02cc96689a87a9e8f71cf8f1 ADLER32:23026e8a</oc:checksum>
        </oc:checksums>
      </d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`

	files, err := parsePROPFIND([]byte(xmlData), "token")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	want := "a7e0206e573edbef0c4d8107a151271fbeccf2fe"
	if files[0].SHA1 != want {
		t.Errorf("expected SHA1 %s, got %q", want, files[0].SHA1)
	}
}

func TestParsePROPFIND_WithoutChecksums(t *testing.T) {
	xmlData := `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/public-files/token/notes.txt</d:href>
    <d:propstat>
      <d:prop>
        <d:getcontentlength>512</d:getcontentlength>
        <d:getcontenttype>text/plain</d:getcontenttype>
      </d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`

	files, err := parsePROPFIND([]byte(xmlData), "token")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].SHA1 != "" {
		t.Errorf("expected empty SHA1, got %q", files[0].SHA1)
	}
}

func TestExtractSHA1(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"SHA1:abc123 MD5:def456 ADLER32:789", "abc123"},
		{"MD5:def456 SHA1:abc123", "abc123"},
		{"MD5:def456 ADLER32:789", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got := extractSHA1(tc.input)
		if got != tc.want {
			t.Errorf("extractSHA1(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
