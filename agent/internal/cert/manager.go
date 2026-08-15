package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const renewalWindow = 30 * 24 * time.Hour

type Manager struct {
	dataDir    string
	baseDomain string
	roots      *x509.CertPool

	mu        sync.RWMutex
	namespace string
	keyPEM    []byte
	chainPEM  []byte
	cert      *tls.Certificate
	leafDER   []byte
}

func NewManager(dataDir, baseDomain string, roots *x509.CertPool) *Manager {
	return &Manager{dataDir: dataDir, baseDomain: baseDomain, roots: roots}
}

func (m *Manager) dir() string      { return filepath.Join(m.dataDir, "direct") }
func (m *Manager) keyPath() string  { return filepath.Join(m.dir(), "key.pem") }
func (m *Manager) chainPath() string { return filepath.Join(m.dir(), "chain.pem") }
func (m *Manager) nsPath() string   { return filepath.Join(m.dir(), "namespace") }

func (m *Manager) SetNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("empty namespace")
	}
	m.mu.Lock()
	m.namespace = namespace
	m.mu.Unlock()
	return m.persistNamespace()
}

func (m *Manager) Namespace() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.namespace
}

func (m *Manager) Load() error {
	if b, err := os.ReadFile(m.nsPath()); err == nil {
		m.mu.Lock()
		m.namespace = string(b)
		m.mu.Unlock()
	}
	keyPEM, err1 := os.ReadFile(m.keyPath())
	chainPEM, err2 := os.ReadFile(m.chainPath())
	if err1 != nil || err2 != nil {
		return nil
	}
	return m.installLocked(chainPEM, keyPEM)
}

func (m *Manager) GenerateCSR() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namespace == "" {
		return nil, fmt.Errorf("namespace not set")
	}
	if m.keyPEM == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		m.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if err := m.persistKey(); err != nil {
			return nil, err
		}
	}
	return CSRFromKey(m.keyPEM, m.namespace, m.baseDomain)
}

func (m *Manager) Install(chainPEM []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyPEM == nil {
		return fmt.Errorf("no local key: generate a CSR first")
	}
	return m.installLocked(chainPEM, m.keyPEM)
}

func (m *Manager) installLocked(chainPEM, keyPEM []byte) error {
	if err := ValidateChain(chainPEM, keyPEM, m.namespace, m.baseDomain, m.roots); err != nil {
		return fmt.Errorf("validate chain: %w", err)
	}
	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return err
	}
	// Persist BEFORE swapping the in-memory cert so a write failure cannot
	// leave a cert active that is not on disk (split runtime/reported state).
	m.chainPEM = chainPEM
	m.keyPEM = keyPEM
	if err := m.persistChain(); err != nil {
		return err
	}
	m.cert = &cert
	m.leafDER = cert.Certificate[0]
	return nil
}

func (m *Manager) Certificate() (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, fmt.Errorf("no certificate installed")
	}
	cp := *m.cert
	return &cp, nil
}

// notAfterLocked reads NotAfter without taking the lock (caller holds it).
func (m *Manager) notAfterLocked() (time.Time, error) {
	if m.cert == nil || len(m.cert.Certificate) == 0 {
		return time.Time{}, fmt.Errorf("no certificate installed")
	}
	c, err := x509.ParseCertificate(m.cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

func (m *Manager) NotAfter() (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.notAfterLocked()
}

func (m *Manager) LeafFingerprint() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.leafDER == nil {
		return "", fmt.Errorf("no certificate installed")
	}
	sum := sha256.Sum256(m.leafDER)
	return hex.EncodeToString(sum[:]), nil
}

func (m *Manager) Installed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert != nil
}

func (m *Manager) NeedsRenewal() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return true
	}
	na, err := m.notAfterLocked()
	if err != nil {
		return true
	}
	return time.Until(na) < renewalWindow
}

func (m *Manager) persistNamespace() error {
	if err := os.MkdirAll(m.dir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.nsPath(), []byte(m.namespace), 0600)
}
func (m *Manager) persistKey() error {
	if err := os.MkdirAll(m.dir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.keyPath(), m.keyPEM, 0600)
}
func (m *Manager) persistChain() error {
	if err := os.MkdirAll(m.dir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.chainPath(), m.chainPEM, 0600)
}
