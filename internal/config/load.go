package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"gopkg.in/yaml.v2"
)

// Load reads configuration from a file path or an (age-encrypted) URL, merges
// any chains.d-style directory, and expands environment variables. password is
// required only when source is an http(s) URL pointing at an age-encrypted file.
func Load(source, chainDir, password string) (*Config, error) {
	c := &Config{}
	var raw []byte
	var err error

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		if password == "" {
			return nil, errors.New("a password is required for remote (encrypted) configuration")
		}
		raw, err = fetchRemote(source, password)
		if err != nil {
			return nil, err
		}
	} else {
		raw, err = os.ReadFile(source) //nolint:gosec -- operator-provided path
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(source, ".age") || looksEncrypted(raw) {
			if password == "" {
				return nil, errors.New("config file is age-encrypted; provide -password or PASSWORD env var")
			}
			raw, err = DecryptAge(raw, password)
			if err != nil {
				return nil, fmt.Errorf("decrypting %s: %w", source, err)
			}
		}
	}
	if err := yaml.UnmarshalStrict(ExpandEnv(raw), c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", source, err)
	}

	if chainDir != "" {
		if err := mergeChainDir(c, chainDir); err != nil {
			return nil, err
		}
	}
	for name, ch := range c.Chains {
		ch.Name = name
	}
	return c, nil
}

func looksEncrypted(b []byte) bool {
	// age files start with the marker line "age-encryption.org/v1"
	return strings.HasPrefix(strings.TrimLeft(string(b), " \t\n"), "age-encryption.org/v1")
}

func fetchRemote(url, password string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching remote config: http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if !looksEncrypted(b) {
		// allow plaintext remote config too — but warn via error-free return
		return b, nil
	}
	return DecryptAge(b, password)
}

// DecryptAge unwraps an age passphrase-encrypted blob (exported for the
// `cometduty decrypt` command).
func DecryptAge(ciphertext []byte, password string) ([]byte, error) {
	id, err := age.NewScryptIdentity(password)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(strings.NewReader(string(ciphertext)), id)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// EncryptAge encrypts plaintext with a passphrase (used by `cometduty encrypt`).
func EncryptAge(plaintext []byte, password string) ([]byte, error) {
	rec, err := age.NewScryptRecipient(password)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	w, err := age.Encrypt(&sb, rec)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// mergeChainDir merges per-chain files: <dir>/<Friendly Name>.yml|yaml, whose
// content is a ChainConfig document (no top-level "chains:" key needed).
func mergeChainDir(c *Config, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		full := filepath.Join(dir, name)
		b, err := os.ReadFile(full) //nolint:gosec -- operator-provided path
		if err != nil {
			return fmt.Errorf("reading %s: %w", full, err)
		}
		cc := &ChainConfig{}
		if err := yaml.UnmarshalStrict(ExpandEnv(b), cc); err != nil {
			return fmt.Errorf("parsing %s: %w", full, err)
		}
		// strip only a trailing .yml/.yaml — keep interior dots in the name
		chainName := name[:len(name)-len(filepath.Ext(name))]
		if c.Chains == nil {
			c.Chains = map[string]*ChainConfig{}
		}
		c.Chains[chainName] = cc
	}
	return nil
}

// LoadState/State helpers are in internal/state.
