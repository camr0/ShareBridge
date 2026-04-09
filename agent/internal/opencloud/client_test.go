package opencloud

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

	files, err := parsePROPFIND([]byte(xml), "/remote.php/dav/public-files/token")
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

	files, err := parsePROPFIND([]byte(xmlData), "/remote.php/dav/public-files/token")
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

	files, err := parsePROPFIND([]byte(xmlData), "/remote.php/dav/public-files/token")
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

func TestParsePROPFIND_WithSubdirectory(t *testing.T) {
	xmlData := `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/public-files/token/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/public-files/token/docs</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/public-files/token/README.txt</d:href>
    <d:propstat>
      <d:prop>
        <d:getcontentlength>1024</d:getcontentlength>
        <d:getcontenttype>text/plain</d:getcontenttype>
      </d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`

	files, err := parsePROPFIND([]byte(xmlData), "/remote.php/dav/public-files/token")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(files))
	}
	var folder, file *FileInfo
	for i := range files {
		if files[i].IsDir {
			folder = &files[i]
		} else {
			file = &files[i]
		}
	}
	if folder == nil {
		t.Fatal("expected a directory entry")
	}
	if folder.Name != "docs" {
		t.Errorf("expected folder name 'docs', got %q", folder.Name)
	}
	if file == nil {
		t.Fatal("expected a file entry")
	}
	if file.Name != "README.txt" {
		t.Errorf("expected file name 'README.txt', got %q", file.Name)
	}
	if file.IsDir {
		t.Error("file entry should not have IsDir=true")
	}
}

func TestParsePROPFIND_EmptyFolder(t *testing.T) {
	xmlData := `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/public-files/token/empty-dir</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`

	files, err := parsePROPFIND([]byte(xmlData), "/remote.php/dav/public-files/token/empty-dir")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 entries for empty folder, got %d", len(files))
	}
}

func TestGetRootFileID_ReturnsFileID(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("expected PROPFIND, got %s", r.Method)
		}
		if r.Header.Get("Depth") != "0" {
			t.Errorf("expected Depth: 0, got %q", r.Header.Get("Depth"))
		}
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte(`<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/public-files/testtoken/</d:href>
    <d:propstat>
      <d:prop>
        <d:resourcetype><d:collection/></d:resourcetype>
        <oc:fileid>storage-users-1$abc!def</oc:fileid>
      </d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	c, err := New(srv.URL+"/s/testtoken", host, "")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	c.httpClient = srv.Client()

	fileID, err := c.GetRootFileID()
	if err != nil {
		t.Fatalf("GetRootFileID() error: %v", err)
	}
	if fileID != "storage-users-1$abc!def" {
		t.Errorf("FileID = %q, want storage-users-1$abc!def", fileID)
	}
}

func TestGetRootFileID_EmptyWhenMissing(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte(`<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/public-files/testtoken/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	c, err := New(srv.URL+"/s/testtoken", host, "")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	c.httpClient = srv.Client()

	fileID, err := c.GetRootFileID()
	if err != nil {
		t.Fatalf("GetRootFileID() should not error when oc:fileid is absent: %v", err)
	}
	if fileID != "" {
		t.Errorf("expected empty fileID for missing oc:fileid, got %q", fileID)
	}
}
