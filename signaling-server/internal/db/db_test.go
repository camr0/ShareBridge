package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestOpen_CreatesTables(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	// Verify tables exist
	var count int
	err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('api_keys', 'sessions')").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestAPIKeyRepo_CreateAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	repo := NewAPIKeyRepo(db)

	// Create key
	hash, _ := bcrypt.GenerateFromPassword([]byte("ak_test.secret123"), bcrypt.DefaultCost)
	err = repo.Create("ak_test", string(hash))
	require.NoError(t, err)

	// Get by ID
	key, err := repo.GetByID("ak_test")
	require.NoError(t, err)
	assert.Equal(t, "ak_test", key.ID)
	assert.True(t, key.IsActive)
}

func TestAPIKeyRepo_Validate(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	repo := NewAPIKeyRepo(db)

	// Create key
	fullKey := "ak_test.secret123"
	hash, _ := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	err = repo.Create("ak_test", string(hash))
	require.NoError(t, err)

	// Validate correct key
	key, err := repo.Validate(fullKey)
	require.NoError(t, err)
	assert.NotNil(t, key)
	assert.Equal(t, "ak_test", key.ID)

	// Validate wrong key
	key, err = repo.Validate("ak_test.wrongsecret")
	assert.NoError(t, err)
	assert.Nil(t, key)
}

func TestSessionRepo_CreateAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// First create an API key (foreign key constraint)
	keyRepo := NewAPIKeyRepo(db)
	hash, _ := bcrypt.GenerateFromPassword([]byte("ak_test.secret"), bcrypt.DefaultCost)
	keyRepo.Create("ak_test", string(hash))

	repo := NewSessionRepo(db)

	// Create session
	session := &Session{
		Code:     "ABC12345",
		APIKeyID: "ak_test",
		AgentID:  "agent-uuid",
		ShareURL: "ocs://example.com/share",
	}
	err = repo.Create(session)
	require.NoError(t, err)

	// Get by code
	got, err := repo.GetByCode("ABC12345")
	require.NoError(t, err)
	assert.Equal(t, "ABC12345", got.Code)
	assert.Equal(t, "ak_test", got.APIKeyID)
	assert.Equal(t, "agent-uuid", got.AgentID)
}

func TestSessionRepo_IsCodeAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	keyRepo := NewAPIKeyRepo(db)
	hash, _ := bcrypt.GenerateFromPassword([]byte("ak_alice.secret"), bcrypt.DefaultCost)
	keyRepo.Create("ak_alice", string(hash))

	repo := NewSessionRepo(db)
	repo.Create(&Session{Code: "TAKEN123", APIKeyID: "ak_alice", ShareURL: "url1"})

	// Code taken by same key
	available, err := repo.IsCodeAvailable("TAKEN123", "ak_alice")
	require.NoError(t, err)
	assert.True(t, available)

	// Code taken by different key
	available, err = repo.IsCodeAvailable("TAKEN123", "ak_mallory")
	require.NoError(t, err)
	assert.False(t, available)

	// Code not taken
	available, err = repo.IsCodeAvailable("FREE1234", "ak_alice")
	require.NoError(t, err)
	assert.True(t, available)
}
