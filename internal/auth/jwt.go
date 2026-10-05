package auth

import (
	"crypto/rsa"
	"fmt"
	"os"
	"strings"
	"time"

	"example.com/subscriptions/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type Claims struct {
	jwt.RegisteredClaims
	Scope string `json:"scope"`
}
type Principal struct {
	User        uuid.UUID
	Read, Write bool
}
type Verifier struct {
	keys   map[string]*rsa.PublicKey
	parser *jwt.Parser
}

func New(c config.Config) (*Verifier, error) {
	if !strings.HasPrefix(c.Issuer, "https://") || c.Audience == "" || len(c.Keys) == 0 {
		return nil, fmt.Errorf("HTTPS issuer, audience and verification keys are required")
	}
	v := &Verifier{keys: make(map[string]*rsa.PublicKey), parser: jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(c.Issuer), jwt.WithAudience(c.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))}
	for _, item := range c.Keys {
		if item.ID == "" || v.keys[item.ID] != nil {
			return nil, fmt.Errorf("duplicate or empty JWT key id")
		}
		b, err := os.ReadFile(item.File)
		if err != nil {
			return nil, fmt.Errorf("read JWT public key %s", item.ID)
		}
		key, err := jwt.ParseRSAPublicKeyFromPEM(b)
		if err != nil || key.N.BitLen() < 2048 {
			return nil, fmt.Errorf("JWT public key %s must be RSA >=2048 bits", item.ID)
		}
		v.keys[item.ID] = key
	}
	return v, nil
}

func (v *Verifier) Verify(raw string) (Principal, error) {
	var result Principal
	if len(raw) > 8192 {
		return result, fmt.Errorf("token too large")
	}
	claims := &Claims{}
	token, err := v.parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || v.keys[kid] == nil {
			return nil, fmt.Errorf("unknown key")
		}
		return v.keys[kid], nil
	})
	if err != nil || !token.Valid {
		return result, fmt.Errorf("invalid token")
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || !claims.ExpiresAt.After(claims.IssuedAt.Time) || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > time.Hour {
		return result, fmt.Errorf("token lifetime must be <=1 hour and iat is required")
	}
	user, err := uuid.Parse(claims.Subject)
	if err != nil || user == uuid.Nil {
		return result, fmt.Errorf("invalid subject")
	}
	result.User = user
	for _, scope := range strings.Fields(claims.Scope) {
		switch scope {
		case "subscriptions:read":
			result.Read = true
		case "subscriptions:write":
			result.Write = true
		}
	}
	return result, nil
}
