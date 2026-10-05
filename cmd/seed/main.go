// seed fills a local development API and writes short-lived tokens for k6.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
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
	users := flag.Int("users", 1000, "number of new local users (1..100000)")
	perUser := flag.Int("per-user", 10, "subscriptions per user (1..100)")
	base := flag.String("url", "http://127.0.0.1:8080", "local development API URL")
	out := flag.String("out", ".local/tokens.json", "new token file, never overwritten")
	flag.Parse()
	u, e := url.Parse(*base)
	if e != nil {
		return e
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || u.User != nil || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("seed accepts only an explicit loopback HTTP address")
	}
	if *users < 1 || *users > 100000 || *perUser < 1 || *perUser > 100 {
		return fmt.Errorf("invalid seed size")
	}
	file, e := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer file.Close()
	pem, e := os.ReadFile(".local/private.pem")
	if e != nil {
		return e
	}
	key, e := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if e != nil {
		return e
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	tokens := make([]string, *users)
	jobs := make(chan int)
	errs := make(chan error, 8)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				user := uuid.New()
				now := time.Now()
				claims := auth.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "https://issuer.local.test", Audience: jwt.ClaimStrings{"subscriptions"}, Subject: user.String(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}, Scope: "subscriptions:read subscriptions:write"}
				t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
				t.Header["kid"] = "local-1"
				token, e := t.SignedString(key)
				if e != nil {
					errs <- e
					cancel()
					return
				}
				tokens[index] = token
				for n := 0; n < *perUser; n++ {
					body := []byte(fmt.Sprintf(`{"service_name":"Service-%d","price":400,"start_date":"01-2025"}`, n))
					request, e := http.NewRequestWithContext(ctx, "POST", *base+"/api/v1/subscriptions", bytes.NewReader(body))
					if e != nil {
						errs <- e
						cancel()
						return
					}
					request.Header.Set("Authorization", "Bearer "+token)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Idempotency-Key", uuid.NewString())
					response, e := client.Do(request)
					if e != nil {
						errs <- e
						cancel()
						return
					}
					io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
					response.Body.Close()
					if response.StatusCode != 201 {
						errs <- fmt.Errorf("seed request returned %d; lower concurrency/load or inspect API logs", response.StatusCode)
						cancel()
						return
					}
				}
			}
		}()
	}
send:
	for i := 0; i < *users; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return e
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := json.NewEncoder(file).Encode(tokens); e != nil {
		return e
	}
	fmt.Printf("Created %d users and %d subscriptions; tokens expire within one hour.\n", *users, *users**perUser)
	return nil
}
