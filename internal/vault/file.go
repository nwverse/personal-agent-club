package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// KeySize is the size of a vault key: AES-256.
const KeySize = 32

// LoadKey reads the key file at path (64 hex digits), creating it with a fresh random key (mode 0600) when create
// is set and it does not exist. A key file that others than its owner can read is refused. A file of exactly 32
// bytes is read as the raw key: the Persona shell wrote its club.key that way before it used this function.
func LoadKey(path string, create bool) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		return newKey(path)
	}
	if err != nil {
		return nil, err
	}
	if err := ownerOnly(path); err != nil {
		return nil, err
	}
	if len(raw) == KeySize {
		return raw, nil
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != KeySize {
		return nil, fmt.Errorf("%s is not a vault key (64 hex digits)", path)
	}
	return key, nil
}

func newKey(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return LoadKey(path, false)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	return key, f.Close()
}

func ownerOnly(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by others (mode %v); it must be 0600", path, fi.Mode().Perm())
	}
	return nil
}

// File is a Vault in one encrypted file: a JSON map of account id to credential, sealed with AES-256-GCM under a
// key kept elsewhere and bound to the space's name. It is read at every Get, so another process (an import) may
// write it. This protects a data directory or backup copied without the key; it does not protect against whoever
// can read both.
type File struct {
	path  string
	aead  cipher.AEAD
	aad   []byte
	mu    sync.Mutex
	space string
}

type sealed struct {
	Version int    `json:"v"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

// OpenFile is the vault of a space at path, under key.
func OpenFile(path string, key []byte, space string) (*File, error) {
	if len(key) != KeySize {
		return nil, errors.New("vault: the key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &File{path: path, aead: aead, aad: []byte("personal-agent-club credentials v1 space=" + space), space: space}, nil
}

func (f *File) read() (map[string]string, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil || s.Version != 1 {
		return nil, fmt.Errorf("vault: %s is not a credentials file", f.path)
	}
	if len(s.Nonce) != f.aead.NonceSize() {
		return nil, fmt.Errorf("vault: %s is damaged", f.path)
	}
	plain, err := f.aead.Open(nil, s.Nonce, s.Data, f.aad)
	if err != nil {
		return nil, fmt.Errorf("vault: %s does not open with this key", f.path)
	}
	creds := map[string]string{}
	if err := json.Unmarshal(plain, &creds); err != nil {
		return nil, fmt.Errorf("vault: %s is damaged", f.path)
	}
	return creds, nil
}

func (f *File) write(creds map[string]string) error {
	plain, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	nonce := make([]byte, f.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out, err := json.Marshal(sealed{Version: 1, Nonce: nonce, Data: f.aead.Seal(nil, nonce, plain, f.aad)})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}

func (f *File) update(change func(map[string]string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	creds, err := f.read()
	if err != nil {
		return err
	}
	change(creds)
	return f.write(creds)
}

// Check reads the file once: an error when it is there and does not open with the key (or is damaged).
func (f *File) Check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.read()
	return err
}

func (f *File) Put(_ context.Context, accountID, credential string) error {
	return f.update(func(m map[string]string) { m[accountID] = credential })
}

func (f *File) Get(_ context.Context, accountID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	creds, err := f.read()
	if err != nil {
		return "", false, err
	}
	c, ok := creds[accountID]
	return c, ok, nil
}

func (f *File) Delete(_ context.Context, accountID string) error {
	return f.update(func(m map[string]string) { delete(m, accountID) })
}

func (f *File) String() string { return fmt.Sprintf("vault.File(space %s)", f.space) }

// GoString keeps %#v from printing the cipher.
func (f *File) GoString() string { return f.String() }
