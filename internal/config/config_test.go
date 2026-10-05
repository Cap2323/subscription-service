package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretSource(t *testing.T) {
	t.Setenv("EXAMPLE_SECRET", "a")
	if v, e := Secret("EXAMPLE_SECRET"); e != nil || v != "a" {
		t.Fatal(e)
	}
	p := filepath.Join(t.TempDir(), "value")
	if e := os.WriteFile(p, []byte("b\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("EXAMPLE_SECRET_FILE", p)
	if _, e := Secret("EXAMPLE_SECRET"); e == nil {
		t.Fatal("accepted conflicting sources")
	}
}
func TestProductionTLS(t *testing.T) {
	c := Config{Profile: "production"}
	s := Shard{DSNEnv: "EXAMPLE_DSN"}
	t.Setenv("EXAMPLE_DSN", "postgres://app:pass@database.test/subscriptions?sslmode=disable")
	if _, e := c.DSN(s, false); e == nil {
		t.Fatal("plaintext production DB accepted")
	}
	t.Setenv("EXAMPLE_DSN", "postgres://app:pass@database.test/subscriptions?sslmode=verify-full")
	if _, e := c.DSN(s, false); e != nil {
		t.Fatal(e)
	}
}
