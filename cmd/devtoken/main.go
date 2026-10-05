// devtoken is a local-development helper. It is absent from the production image.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"time"

	"example.com/subscriptions/internal/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	init := flag.Bool("init", false, "create local keys and .env (never overwrite)")
	sub := flag.String("user", "60601fee-2bf1-4721-ae6f-7636e79a0cba", "user UUID")
	keyPath := flag.String("key", ".local/private.pem", "local private key")
	flag.Parse()
	if *init {
		return initialize()
	}
	id, e := uuid.Parse(*sub)
	if e != nil || id == uuid.Nil {
		return fmt.Errorf("invalid user UUID")
	}
	b, e := os.ReadFile(*keyPath)
	if e != nil {
		return e
	}
	key, e := jwt.ParseRSAPrivateKeyFromPEM(b)
	if e != nil {
		return e
	}
	now := time.Now()
	claims := auth.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "https://issuer.local.test", Audience: jwt.ClaimStrings{"subscriptions"}, Subject: id.String(), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)), ID: uuid.NewString()}, Scope: "subscriptions:read subscriptions:write"}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "local-1"
	raw, e := token.SignedString(key)
	if e != nil {
		return e
	}
	fmt.Println(raw)
	return nil
}
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if e != nil {
		return e
	}
	if _, e := f.Write(b); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
func secret() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func initialize() error {
	for _, p := range []string{".env", ".local/private.pem", ".local/public.pem"} {
		if _, e := os.Stat(p); e == nil {
			return fmt.Errorf("%s exists; refusing to replace secrets", p)
		}
	}
	if e := os.MkdirAll(".local", 0755); e != nil {
		return e
	}
	key, e := rsa.GenerateKey(rand.Reader, 3072)
	if e != nil {
		return e
	}
	public, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		return e
	}
	if e := writeNew(".local/private.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); e != nil {
		return e
	}
	if e := writeNew(".local/public.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}), 0644); e != nil {
		return e
	}
	admin, migrator, app := secret(), secret(), secret()
	env := fmt.Sprintf("PG_ADMIN_PASSWORD=%s\nPG_MIGRATOR_PASSWORD=%s\nPG_APP_PASSWORD=%s\nCURSOR_KEY=%s\nGRAFANA_PASSWORD=%s\n", admin, migrator, app, secret(), secret())
	env += "API_CONFIG_PATH=configs/docker.json\n"
	for i, id := range []string{"A", "B", "C"} {
		env += fmt.Sprintf("SHARD_%s_DSN=postgres://subscriptions_app:%s@127.0.0.1:%d/subscriptions?sslmode=disable\nSHARD_%s_ADMIN_DSN=postgres://subscriptions_migrator:%s@127.0.0.1:%d/subscriptions?sslmode=disable\n", id, app, 5433+i, id, migrator, 5433+i)
	}
	if e := writeNew(".env", []byte(env), 0600); e != nil {
		return e
	}
	fmt.Println("Created .env and local RSA keys. Never commit them.")
	return nil
}
