package hub

import (
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestAgentReplacementFencesOldSocketAndCleanup(t *testing.T) {
	h := New()
	old, current := new(websocket.Conn), new(websocket.Conn)
	h.RegisterAgent("owner", old)
	h.RegisterCode("share", "owner")
	require.NoError(t, h.PairSession("share", "old-browser", "owner", new(websocket.Conn)))
	h.RegisterAgent("owner", current)
	require.False(t, h.IsCurrentAgent("owner", old))
	require.True(t, h.IsCurrentAgent("owner", current))
	_, oldAuthorized := h.BrowserSessionForAgent("owner", old, "old-browser")
	require.False(t, oldAuthorized)
	require.Zero(t, h.IncrementAuthFailure("owner", old, "old-browser"))
	require.Zero(t, h.IncrementImmichAuthFailure("owner", old, "old-browser"))
	h.UnregisterAgent("owner", old)
	_, hasOldWriter := h.connWrites[old]
	require.False(t, hasOldWriter)
	require.True(t, h.IsCurrentAgent("owner", current), "old disconnect removed the replacement")
	require.NoError(t, h.PairSession("share", "new-browser", "owner", new(websocket.Conn)))
	code, ok := h.BrowserSessionForAgent("owner", current, "new-browser")
	require.True(t, ok)
	require.Equal(t, "share", code)
	h.UnregisterAgent("owner", current)
	require.False(t, h.IsCurrentAgent("owner", current))
	_, ok = h.BrowserSessionForAgent("owner", current, "new-browser")
	require.False(t, ok)
}

func TestBrowserBindingsKeepEachSharesOwner(t *testing.T) {
	h := New()
	owner, other := new(websocket.Conn), new(websocket.Conn)
	h.RegisterAgent("owner", owner)
	h.RegisterAgent("other", other)
	h.RegisterCode("share", "owner")
	require.Error(t, h.PairSession("share", "forged-browser", "other", new(websocket.Conn)))
	for _, id := range []string{"first", "second"} {
		require.NoError(t, h.PairSession("share", id, "owner", new(websocket.Conn)))
		code, ok := h.BrowserSessionForAgent("owner", owner, id)
		require.True(t, ok)
		require.Equal(t, "share", code)
	}
	for _, id := range []string{"first", "second", "unknown"} {
		_, ok := h.BrowserSessionForAgent("other", other, id)
		require.False(t, ok)
		require.Zero(t, h.IncrementAuthFailure("other", other, id))
		require.Zero(t, h.IncrementImmichAuthFailure("other", other, id))
	}
	require.Empty(t, h.connFails)
	require.Empty(t, h.connImmichFails)
	h.UnregisterBrowserConn("first")
	_, ok := h.BrowserSessionForAgent("owner", owner, "first")
	require.False(t, ok)
	require.NotContains(t, h.browserOwners, "first")
	_, ok = h.BrowserSessionForAgent("owner", owner, "second")
	require.True(t, ok)
	h.RegisterCode("share", "other")
	_, ok = h.BrowserSessionForAgent("owner", owner, "second")
	require.False(t, ok, "a reassigned share retained its old owner")
}

func TestAgentReplacementRebindsOwnedBrowser(t *testing.T) {
	h := New()
	old, current := new(websocket.Conn), new(websocket.Conn)
	h.RegisterAgent("owner", old)
	h.RegisterCode("share", "owner")
	require.NoError(t, h.PairSession("share", "browser", "owner", new(websocket.Conn)))
	h.RegisterAgent("owner", current)
	code, ok := h.BrowserSessionForAgent("owner", current, "browser")
	require.True(t, ok)
	require.Equal(t, "share", code)
	require.Same(t, current, h.pairs["share"].agentConn)
}
