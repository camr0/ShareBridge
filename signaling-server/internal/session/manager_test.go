package session_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"opencloudshare/server/internal/session"
)

func TestCreate_ReturnsSessionWithEightCharCode(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("tok", "https://example.com/s/ABC", "", 24*time.Hour)
	require.NoError(t, err)
	assert.Len(t, s.ID, 8)
	assert.Equal(t, "tok", s.Token)
	assert.Equal(t, "https://example.com/s/ABC", s.ShareURL)
	assert.True(t, s.ExpiresAt.After(time.Now()))
}

func TestCreate_CodesAreUnique(t *testing.T) {
	m := session.NewManager()
	codes := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := m.Create("tok", "url", "", time.Hour)
		require.NoError(t, err)
		codes[s.ID] = true
	}
	assert.Len(t, codes, 100)
}

func TestGet_ReturnsExistingSession(t *testing.T) {
	m := session.NewManager()
	s, _ := m.Create("tok", "url", "", time.Hour)
	got, ok := m.Get(s.ID)
	assert.True(t, ok)
	assert.Equal(t, s.ID, got.ID)
}

func TestGet_ReturnsFalseForExpired(t *testing.T) {
	m := session.NewManager()
	s, _ := m.Create("tok", "url", "", -time.Second) // already expired
	_, ok := m.Get(s.ID)
	assert.False(t, ok)
}

func TestGet_ReturnsFalseForUnknown(t *testing.T) {
	m := session.NewManager()
	_, ok := m.Get("notexist")
	assert.False(t, ok)
}

func TestDelete_RemovesSession(t *testing.T) {
	m := session.NewManager()
	s, _ := m.Create("tok", "url", "", time.Hour)
	m.Delete(s.ID)
	_, ok := m.Get(s.ID)
	assert.False(t, ok)
}

// TestCreate_EmptyPreferredCode verifies that when preferredCode is empty,
// a random 8-character alphanumeric code is generated.
func TestCreate_EmptyPreferredCode(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("tok", "url", "", time.Hour)
	require.NoError(t, err)
	assert.Len(t, s.ID, 8)
	// Verify code is alphanumeric (contains only lowercase letters and digits)
	for _, c := range s.ID {
		assert.True(t, (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'), "code should be alphanumeric")
	}
}

// TestCreate_PreferredCode_Free verifies that when preferredCode is provided
// and not taken, that code is used.
func TestCreate_PreferredCode_Free(t *testing.T) {
	m := session.NewManager()
	preferredCode := "mycode12"
	s, err := m.Create("tok", "url", preferredCode, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, preferredCode, s.ID)
}

// TestCreate_PreferredCode_SameToken verifies that when the same token
// re-registers with the same code, the code is reused.
func TestCreate_PreferredCode_SameToken(t *testing.T) {
	m := session.NewManager()
	token := "sametoken"
	preferredCode := "samecode"

	// First registration
	s1, err := m.Create(token, "url1", preferredCode, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, preferredCode, s1.ID)

	// Second registration with same token and code
	s2, err := m.Create(token, "url2", preferredCode, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, preferredCode, s2.ID)
}

// TestCreate_PreferredCode_Taken verifies that when preferredCode is already
// taken by a different token, a random code is generated instead.
func TestCreate_PreferredCode_Taken(t *testing.T) {
	m := session.NewManager()
	preferredCode := "wantedcode"

	// First token takes the code
	s1, err := m.Create("token1", "url1", preferredCode, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, preferredCode, s1.ID)

	// Second token tries to use the same code
	s2, err := m.Create("token2", "url2", preferredCode, time.Hour)
	require.NoError(t, err)
	assert.NotEqual(t, preferredCode, s2.ID, "expected different code when preferred is taken")
	assert.Len(t, s2.ID, 8)
}
