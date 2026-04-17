package identity

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// LoadOrCreate returns an Ed25519 libp2p private key persisted at path.
// The key is created on first call; subsequent calls return the same key.
// Directory is created with mode 0700; the file is written with mode 0600.
func LoadOrCreate(path string) (crypto.PrivKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		priv, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse identity key at %s: %w", path, err)
		}
		return priv, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read identity key at %s: %w", path, err)
	}

	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, fmt.Errorf("generate identity key: %w", err)
	}

	data, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal identity key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create identity dir: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return nil, fmt.Errorf("write identity key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("rename identity key: %w", err)
	}
	return priv, nil
}