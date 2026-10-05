package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"example.com/subscriptions/internal/hashring"
)

type Shard struct {
	ID          string `json:"id"`
	DSNEnv      string `json:"dsn_env"`
	AdminDSNEnv string `json:"admin_dsn_env"`
}
type Key struct {
	ID   string `json:"id"`
	File string `json:"file"`
}
type Config struct {
	Profile       string  `json:"profile"`
	HTTPAddr      string  `json:"http_addr"`
	AdminAddr     string  `json:"admin_addr"`
	Issuer        string  `json:"issuer"`
	Audience      string  `json:"audience"`
	Keys          []Key   `json:"keys"`
	CursorKeyEnv  string  `json:"cursor_key_env"`
	Epoch         int64   `json:"epoch"`
	Buckets       int     `json:"buckets"`
	Shards        []Shard `json:"shards"`
	PoolMax       int32   `json:"pool_max"`
	MaxConcurrent int     `json:"max_concurrent"`
	RPS           int     `json:"rps"`
}

func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration must contain one object")
	}
	if c.Profile != "development" && c.Profile != "production" {
		return c, fmt.Errorf("profile must be development or production")
	}
	if c.PoolMax < 1 || c.PoolMax > 100 || c.MaxConcurrent < 1 || c.MaxConcurrent > 4096 || c.RPS < 1 || c.RPS > 100000 {
		return c, fmt.Errorf("invalid pool or request limits")
	}
	for _, a := range []string{c.HTTPAddr, c.AdminAddr} {
		if _, _, err := net.SplitHostPort(a); err != nil {
			return c, fmt.Errorf("invalid listen address")
		}
	}
	if c.HTTPAddr == c.AdminAddr {
		return c, fmt.Errorf("HTTP and admin addresses must differ")
	}
	if _, err := c.Ring(); err != nil {
		return c, err
	}
	for _, s := range c.Shards {
		if s.DSNEnv == "" || s.AdminDSNEnv == "" || s.DSNEnv == s.AdminDSNEnv {
			return c, fmt.Errorf("separate runtime and admin DSN variables are required")
		}
	}
	return c, nil
}
func (c Config) Ring() (*hashring.Ring, error) {
	ids := make([]string, len(c.Shards))
	for i, s := range c.Shards {
		ids[i] = s.ID
	}
	return hashring.New(hashring.Spec{Epoch: c.Epoch, Buckets: c.Buckets, Shards: ids})
}
func Secret(name string) (string, error) {
	v, has := os.LookupEnv(name)
	file, hasFile := os.LookupEnv(name + "_FILE")
	if has && hasFile {
		return "", fmt.Errorf("set only %s or %s_FILE", name, name)
	}
	if hasFile {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("cannot read %s_FILE", name)
		}
		if len(b) > 65536 {
			return "", fmt.Errorf("secret too large")
		}
		v = strings.TrimSpace(string(b))
	}
	if v == "" {
		return "", fmt.Errorf("missing %s or %s_FILE", name, name)
	}
	return v, nil
}
func (c Config) DSN(s Shard, admin bool) (string, error) {
	name := s.DSNEnv
	if admin {
		name = s.AdminDSNEnv
	}
	value, err := Secret(name)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", fmt.Errorf("%s must be a PostgreSQL URL", name)
	}
	if c.Profile == "production" && u.Query().Get("sslmode") != "verify-full" {
		return "", fmt.Errorf("production requires sslmode=verify-full for %s", name)
	}
	return value, nil
}
func (c Config) CursorKey() ([]byte, error) {
	v, e := Secret(c.CursorKeyEnv)
	if e != nil {
		return nil, e
	}
	b, e := hex.DecodeString(v)
	if e != nil || len(b) != 32 {
		return nil, fmt.Errorf("cursor key must be 32 bytes encoded as 64 hex characters")
	}
	return b, nil
}
