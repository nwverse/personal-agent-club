package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemory(t *testing.T) {
	ctx := context.Background()
	v := NewMemory()
	if _, ok, _ := v.Get(ctx, "a000000000001"); ok {
		t.Fatal("empty vault has a credential")
	}
	if err := v.Put(ctx, "a000000000001", "secret-value-1"); err != nil {
		t.Fatal(err)
	}
	if c, ok, _ := v.Get(ctx, "a000000000001"); !ok || c != "secret-value-1" {
		t.Fatalf("got %q %v", c, ok)
	}
	for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
		if strings.Contains(s, "secret-value-1") {
			t.Fatalf("formatting leaks the credential: %s", s)
		}
	}
	_ = v.Delete(ctx, "a000000000001")
	if _, ok, _ := v.Get(ctx, "a000000000001"); ok {
		t.Fatal("deleted credential still there")
	}
}

func TestFileVault(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	key, err := LoadKey(keyPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode())
	}
	again, err := LoadKey(keyPath, true)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatal("the key changed on the second load", err)
	}
	path := filepath.Join(dir, "spaces", "default", "credentials.enc")
	v, err := OpenFile(path, key, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := v.Get(ctx, "a000000000001"); ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := v.Put(ctx, "a000000000001", "secret-value-1"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(ctx, "a000000000002", "secret-value-2"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("secret-value")) || bytes.Contains(raw, []byte("a000000000001")) {
		t.Fatalf("the file is not encrypted: %s", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	other, _ := OpenFile(path, key, "default")
	if c, ok, _ := other.Get(ctx, "a000000000001"); !ok || c != "secret-value-1" {
		t.Fatalf("a second reader sees %q", c)
	}
	_ = v.Delete(ctx, "a000000000001")
	if _, ok, _ := other.Get(ctx, "a000000000001"); ok {
		t.Fatal("deleted credential still there")
	}
	wrong := make([]byte, KeySize)
	if w, _ := OpenFile(path, wrong, "default"); w != nil {
		if _, _, err := w.Get(ctx, "a000000000002"); err == nil {
			t.Fatal("opened with the wrong key")
		}
	}
	moved, _ := OpenFile(path, key, "other")
	if _, _, err := moved.Get(ctx, "a000000000002"); err == nil {
		t.Fatal("opened as another space")
	}
	for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%#v", v), fmt.Sprintf("%+v", v)} {
		if strings.Contains(s, "secret") || strings.Contains(s, hex.EncodeToString(key)) {
			t.Fatalf("formatting leaks: %s", s)
		}
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(keyPath, true); err == nil {
		t.Fatal("a key readable by others was accepted")
	}
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(bad, []byte("short"), 0o600)
	if _, err := LoadKey(bad, false); err == nil {
		t.Fatal("a short key was accepted")
	}
	if _, err := LoadKey(filepath.Join(dir, "missing"), false); err == nil {
		t.Fatal("a missing key was created")
	}
	// the raw 32 bytes the Persona shell's own key file once held
	rawPath := filepath.Join(dir, "club.key")
	_ = os.WriteFile(rawPath, key, 0o600)
	if got, err := LoadKey(rawPath, true); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("a raw key: %v", err)
	}
	if again, _ := os.ReadFile(rawPath); !bytes.Equal(again, key) {
		t.Fatal("a raw key file was rewritten")
	}
}

func TestFileCheckRejectsMalformedNonce(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "missing", raw: `{"v":1,"data":""}`},
		{name: "null", raw: `{"v":1,"nonce":null,"data":""}`},
		{name: "short", raw: `{"v":1,"nonce":"AA==","data":""}`},
		{name: "long", raw: `{"v":1,"nonce":"AAAAAAAAAAAAAAAAAA==","data":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.enc")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			v, err := OpenFile(path, make([]byte, KeySize), "default")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("Check panicked: %v", recovered)
				}
			}()
			if err := v.Check(); err == nil || !strings.HasSuffix(err.Error(), "is damaged") {
				t.Fatalf("Check error = %v, want damaged vault error", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.raw {
				t.Fatal("Check changed the damaged vault")
			}
		})
	}
}
