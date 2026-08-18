// agent/internal/direct/items_test.go
package direct

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"sharebridge/agent/internal/immich"
)

func itemsTestGallery() immich.Gallery {
	dur := 12.5
	return immich.Gallery{
		AlbumName:        "Summer",
		AlbumDescription: "vacation",
		Items: []immich.GalleryItem{
			{ID: "a1", Name: "one.jpg", MimeType: "image/jpeg", Width: 4032, Height: 3024, Size: 4200000, Duration: &dur, SHA1: "abc123"},
			{ID: "a2", Name: "two.png", MimeType: "image/png", Width: 100, Height: 200, Size: 1000, Duration: nil, SHA1: "def456"},
		},
	}
}

func newItemsHandler(t *testing.T, gallery immich.Gallery) http.Handler {
	t.Helper()
	return newHandlerServerResolve(t, &fakeResolver{sessions: map[string]*ContentSession{
		"abc": {
			Backend:    &handlerBackend{},
			Gallery:    gallery,
			Membership: map[string]struct{}{"a1": {}, "a2": {}},
		},
	}})
}

func TestItemsReturnsLowerCamelJSON(t *testing.T) {
	handler := newItemsHandler(t, itemsTestGallery())

	rr := doRequest(handler, http.MethodGet, "/s/abc/items")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var got struct {
		AlbumName        string `json:"albumName"`
		AlbumDescription string `json:"albumDescription"`
		Items            []struct {
			ID       string   `json:"id"`
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Width    int      `json:"width"`
			Height   int      `json:"height"`
			Size     int64    `json:"size"`
			Duration *float64 `json:"duration"`
			SHA1     string   `json:"sha1"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AlbumName != "Summer" || got.AlbumDescription != "vacation" {
		t.Fatalf("album = %q/%q, want Summer/vacation", got.AlbumName, got.AlbumDescription)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	first := got.Items[0]
	if first.ID != "a1" || first.Name != "one.jpg" || first.MimeType != "image/jpeg" ||
		first.Width != 4032 || first.Height != 3024 || first.Size != 4200000 ||
		first.SHA1 != "abc123" {
		t.Fatalf("first item = %+v", first)
	}
	if first.Duration == nil || *first.Duration != 12.5 {
		t.Fatalf("first item duration = %v, want 12.5", first.Duration)
	}
	if got.Items[1].Duration != nil {
		t.Fatalf("second item duration = %v, want nil", got.Items[1].Duration)
	}
	// The wire body must carry lowerCamel keys only; a nil duration must
	// serialize as an explicit null (never an absent/PascalCase key).
	body := rr.Body.String()
	for _, bad := range []string{`"AlbumName"`, `"album_name"`, `"MimeType"`, `"mime_type"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("body contains unexpected key %q: %s", bad, body)
		}
	}
	if !strings.Contains(body, `"duration":null`) {
		t.Fatalf("nil duration must serialize as null: %s", body)
	}
}

func TestItemsEmptyAlbumReturnsEmptyArray(t *testing.T) {
	handler := newItemsHandler(t, immich.Gallery{})

	rr := doRequest(handler, http.MethodGet, "/s/abc/items")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Fatalf("empty album: want \"items\":[], got %s", rr.Body.String())
	}
}

func TestItemsHeadReturnsHeadersNoBody(t *testing.T) {
	handler := newItemsHandler(t, itemsTestGallery())

	rr := doRequest(handler, http.MethodHead, "/s/abc/items")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
