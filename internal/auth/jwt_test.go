package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/subscriptions/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestJWTValidation(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	b, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if e := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}), 0600); e != nil {
		t.Fatal(e)
	}
	c := config.Config{Issuer: "https://issuer.test", Audience: "subscriptions", Keys: []config.Key{{ID: "key1", File: path}}}
	v, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	id := uuid.New()
	base := Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: c.Issuer, Audience: jwt.ClaimStrings{c.Audience}, Subject: id.String(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute))}, Scope: "subscriptions:read"}
	sign := func(claims Claims, kid string, method jwt.SigningMethod, signer any) string {
		token := jwt.NewWithClaims(method, claims)
		token.Header["kid"] = kid
		raw, e := token.SignedString(signer)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	p, e := v.Verify(sign(base, "key1", jwt.SigningMethodRS256, key))
	if e != nil || p.User != id || !p.Read || p.Write {
		t.Fatal(p, e)
	}
	for _, tt := range []struct {
		name   string
		change func(*Claims)
	}{
		{"issuer", func(c *Claims) { c.Issuer = "https://evil.test" }}, {"audience", func(c *Claims) { c.Audience = jwt.ClaimStrings{"other"} }},
		{"expired", func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Hour)) }}, {"no expiry", func(c *Claims) { c.ExpiresAt = nil }},
		{"no iat", func(c *Claims) { c.IssuedAt = nil }}, {"future iat", func(c *Claims) { c.IssuedAt = jwt.NewNumericDate(now.Add(5 * time.Minute)) }},
		{"long lifetime", func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(2 * time.Hour)) }}, {"nil subject", func(c *Claims) { c.Subject = uuid.Nil.String() }},
		{"nbf", func(c *Claims) { c.NotBefore = jwt.NewNumericDate(now.Add(5 * time.Minute)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := base
			tt.change(&claims)
			if _, e := v.Verify(sign(claims, "key1", jwt.SigningMethodRS256, key)); e == nil {
				t.Fatal("accepted invalid token")
			}
		})
	}
	for _, raw := range []string{sign(base, "unknown", jwt.SigningMethodRS256, key), sign(base, "key1", jwt.SigningMethodHS256, b), "not.a.token"} {
		if _, e := v.Verify(raw); e == nil {
			t.Fatal("accepted invalid signature/algorithm")
		}
	}
}
