package web

import "testing"

func TestDerivePublicURLUsesImmichGalleryRoute(t *testing.T) {
	got := derivePublicURL("wss://sharebridge.app/ws", "IMMICHKEY", "immich")
	want := "https://sharebridge.app/i/IMMICHKEY"
	if got != want {
		t.Fatalf("derivePublicURL() = %q, want %q", got, want)
	}
}

func TestDerivePublicURLKeepsFileRouteForNonImmichShares(t *testing.T) {
	got := derivePublicURL("wss://sharebridge.app/ws", "FILECODE", "opencloud")
	want := "https://sharebridge.app/s/FILECODE"
	if got != want {
		t.Fatalf("derivePublicURL() = %q, want %q", got, want)
	}
}
