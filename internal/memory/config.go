// Package memory keeps coding-session observations in a user-owned Redis store.
// It deliberately has no dependency on Conductor's shared coordination API.
package memory

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrDisabled = errors.New("Conductor memory is disabled; run `conductor memory configure` first")

type Config struct {
	RedisURL  string `json:"redis_url"`
	Cluster   bool   `json:"cluster"`
	Namespace string `json:"namespace"`
	Disabled  bool   `json:"disabled,omitempty"`
}

func stateDir() (string, error) {
	if dir := os.Getenv("CONDUCTOR_STATE_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor"), nil
}

func configPaths() (string, string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "memory.json"), filepath.Join(dir, "memory.key"), nil
}

func LoadConfig() (Config, [32]byte, error) {
	var cfg Config
	var key [32]byte
	path, keyPath, err := configPaths()
	if err != nil {
		return cfg, key, err
	}
	if body, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return cfg, key, fmt.Errorf("invalid memory config: %w", err)
		}
	} else if !os.IsNotExist(readErr) {
		return cfg, key, readErr
	}
	if v := os.Getenv("CONDUCTOR_MEMORY_REDIS_URL"); v != "" {
		cfg.RedisURL = v
	}
	if v := os.Getenv("CONDUCTOR_MEMORY_NAMESPACE"); v != "" {
		cfg.Namespace = v
	}
	if v := os.Getenv("CONDUCTOR_MEMORY_REDIS_CLUSTER"); v != "" {
		cfg.Cluster = v == "1" || strings.EqualFold(v, "true")
	}
	if cfg.Disabled {
		return cfg, key, ErrDisabled
	}
	if cfg.RedisURL == "" {
		return cfg, key, ErrDisabled
	}
	if err := validateURL(cfg.RedisURL, cfg.Cluster); err != nil {
		return cfg, key, err
	}
	if cfg.Namespace == "" {
		return cfg, key, errors.New("memory namespace missing; run `conductor memory configure`")
	}
	if v := os.Getenv("CONDUCTOR_MEMORY_KEY"); v != "" {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(decoded) != 32 {
			return cfg, key, errors.New("CONDUCTOR_MEMORY_KEY must be base64 for exactly 32 bytes")
		}
		copy(key[:], decoded)
		return cfg, key, nil
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		return cfg, key, fmt.Errorf("memory key unavailable: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return cfg, key, errors.New("memory key file must be owner-only (chmod 600)")
	}
	body, err := os.ReadFile(keyPath)
	if err != nil {
		return cfg, key, fmt.Errorf("memory key unavailable: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil || len(decoded) != 32 {
		return cfg, key, errors.New("invalid memory key file")
	}
	copy(key[:], decoded)
	return cfg, key, nil
}

// Configure opts this machine in. A new key and namespace are generated only once.
// A password-bearing URL should be supplied via stdin rather than a shell argument.
func Configure(redisURL string, cluster bool) error {
	path, keyPath, err := configPaths()
	if err != nil {
		return err
	}
	if err := validateURL(redisURL, cluster); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var cfg Config
	if body, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if cfg.Namespace == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		cfg.Namespace = base64.RawURLEncoding.EncodeToString(b)
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(base64.StdEncoding.EncodeToString(b) + "\n"); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	cfg.RedisURL, cfg.Cluster, cfg.Disabled = redisURL, cluster, false
	return writeConfig(path, cfg)
}

func writeConfig(path string, cfg Config) error {
	dir := filepath.Dir(path)
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".memory-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Disable stops hook capture and recall without deleting encrypted observations or the key.
func Disable() error {
	path, _, err := configPaths()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// An environment-only setup is enabled without a config file. Persisting
		// this flag must still stop its hooks on subsequent invocations.
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return writeConfig(path, Config{Disabled: true})
	}
	if err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return err
	}
	cfg.Disabled = true
	return writeConfig(path, cfg)
}

func validateURL(raw string, cluster bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return errors.New("memory Redis URL must use redis:// or rediss://")
	}
	if _, ok := u.Query()["skip_verify"]; ok {
		return errors.New("memory Redis TLS certificate verification cannot be disabled")
	}
	if u.Hostname() == "" {
		return errors.New("memory Redis URL needs a host")
	}
	if u.Scheme == "redis" && !isLoopback(u.Hostname()) {
		return errors.New("remote Redis requires rediss:// with TLS")
	}
	if cluster && u.Path != "" && u.Path != "/" && u.Path != "/0" {
		return errors.New("Redis cluster mode supports only database 0")
	}
	if cluster {
		if db, ok := u.Query()["db"]; ok && (len(db) != 1 || db[0] != "0") {
			return errors.New("Redis cluster mode supports only database 0")
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func newClient(cfg Config) (redis.UniversalClient, error) {
	if err := validateURL(cfg.RedisURL, cfg.Cluster); err != nil {
		return nil, err
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	if opts.TLSConfig != nil {
		if opts.TLSConfig.InsecureSkipVerify {
			return nil, errors.New("memory Redis TLS certificate verification cannot be disabled")
		}
		opts.TLSConfig.MinVersion = tls.VersionTLS12
	}
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second
	if !cfg.Cluster {
		return redis.NewClient(opts), nil
	}
	if opts.DB != 0 {
		return nil, errors.New("Redis cluster mode supports only database 0")
	}
	return redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        []string{opts.Addr},
		Username:     opts.Username,
		Password:     opts.Password,
		TLSConfig:    opts.TLSConfig,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
	}), nil
}
