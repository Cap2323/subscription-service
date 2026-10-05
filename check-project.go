//go:build ignore

// Run from the project root: go run ./check-project.go
// Requires the local API at 127.0.0.1:8080 and cmd/devtoken's local RSA keys.
// Uses new random test subjects and removes their test subscriptions on exit.
// Does not drop tables, change topology, or print tokens/private keys.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const rootURL = "http://127.0.0.1:8080/api/v1/subscriptions"

type subscription struct {
	ID      string  `json:"id"`
	UserID  string  `json:"user_id"`
	Price   int64   `json:"price"`
	Version int64   `json:"version"`
	EndDate *string `json:"end_date"`
}
type page struct {
	Items []subscription `json:"items"`
	Next  string         `json:"next_cursor"`
}
type reply struct {
	Code   int
	Header http.Header
	Body   []byte
}
type suite struct {
	client             *http.Client
	a, b, userA, userB string
	passed             int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		fmt.Fprintln(os.Stderr, "Diagnostics: docker compose logs --tail=100 app migrate init-topology")
		os.Exit(1)
	}
}
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
func tokenFor(user string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/devtoken", "-user", user)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("devtoken failed; run from project root and check Go/local keys: %s", strings.TrimSpace(stderr.String()))
	}
	token := strings.TrimSpace(string(out))
	if len(strings.Split(token, ".")) != 3 {
		return "", fmt.Errorf("devtoken did not return a JWT")
	}
	return token, nil
}
func run() (result error) {
	for _, path := range []string{"go.mod", ".local/private.pem", "cmd/devtoken/main.go"} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("missing %s; run this file from the initialized project root", path)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Local smoke test must not forward tokens to a proxy.
	defer transport.CloseIdleConnections()
	s := &suite{client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var err error
	if s.userA, err = newUUID(); err != nil {
		return err
	}
	if s.userB, err = newUUID(); err != nil {
		return err
	}
	fmt.Println("Local API smoke test. No database reset; random test users only.")
	fmt.Println("Test user A:", s.userA)
	fmt.Println("Test user B:", s.userB)
	if s.a, err = tokenFor(s.userA); err != nil {
		return err
	}
	if s.b, err = tokenFor(s.userB); err != nil {
		return err
	}
	defer func() {
		if err := s.cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "Cleanup warning:", err)
			if result == nil {
				result = err
			}
		}
		if result == nil {
			fmt.Printf("PASS: %d API checks. Test subscriptions removed.\n", s.passed)
		}
	}()
	return s.test()
}
func (s *suite) request(method, path, token, body string, headers map[string]string) (reply, error) {
	var result reply
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, rootURL+path, reader)
	if err != nil {
		return result, err
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := s.client.Do(r)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return result, err
	}
	if len(data) > 1<<20 {
		return result, fmt.Errorf("response exceeds 1 MiB")
	}
	return reply{resp.StatusCode, resp.Header.Clone(), data}, nil
}
func (s *suite) check(name, method, path, token, body string, headers map[string]string, want int, verify func(reply) error) (reply, error) {
	r, err := s.request(method, path, token, body, headers)
	if err != nil {
		return r, fmt.Errorf("%s: %w", name, err)
	}
	if r.Code != want {
		var apiError struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(r.Body, &apiError)
		return r, fmt.Errorf("%s: HTTP %d, expected %d; code=%s request_id=%s", name, r.Code, want, apiError.Code, apiError.RequestID)
	}
	if verify != nil {
		if err := verify(r); err != nil {
			return r, fmt.Errorf("%s: %w", name, err)
		}
	}
	s.passed++
	fmt.Printf("OK %02d | %s\n", s.passed, name)
	return r, nil
}
func (s *suite) status(name, method, path, token, body string, headers map[string]string, want int) error {
	_, err := s.check(name, method, path, token, body, headers, want, nil)
	return err
}
func require(ok bool, message string) error {
	if !ok {
		return fmt.Errorf("%s", message)
	}
	return nil
}
func (s *suite) expectList(name, token string, want int) error {
	_, err := s.check(name, "GET", "?limit=100", token, "", nil, 200, func(r reply) error {
		var p page
		if err := json.Unmarshal(r.Body, &p); err != nil {
			return err
		}
		return require(len(p.Items) == want, fmt.Sprintf("got %d subscriptions, expected %d", len(p.Items), want))
	})
	return err
}
func (s *suite) expectTotal(name string, want int64) error {
	_, err := s.check(name, "GET", "/total?from=07-2025&to=09-2025", s.a, "", nil, 200, func(r reply) error {
		var total struct {
			Total *int64 `json:"total"`
		}
		if err := json.Unmarshal(r.Body, &total); err != nil {
			return err
		}
		return require(total.Total != nil && *total.Total == want, fmt.Sprintf("expected total %d", want))
	})
	return err
}
func (s *suite) test() error {
	if err := s.status("Missing JWT rejected", "GET", "", "", "", nil, 401); err != nil {
		return err
	}
	if err := s.status("Invalid JWT rejected", "GET", "", s.a+"x", "", nil, 401); err != nil {
		return err
	}
	if err := s.expectList("New user has an empty list", s.a, 0); err != nil {
		return err
	}
	key, err := newUUID()
	if err != nil {
		return err
	}
	headers := map[string]string{"Idempotency-Key": key}
	body := `{"service_name":"Smoke Main","price":400,"start_date":"05-2025"}`
	var first subscription
	_, err = s.check("Create subscription", "POST", "", s.a, body, headers, 201, func(r reply) error {
		if err := json.Unmarshal(r.Body, &first); err != nil {
			return err
		}
		return require(first.ID != "" && first.UserID == s.userA && first.Version == 1 && first.Price == 400 && r.Header.Get("ETag") == `"1"`, "incorrect created data/owner/version/ETag")
	})
	if err != nil {
		return err
	}
	_, err = s.check("Repeated POST returns the same ID", "POST", "", s.a, body, headers, 201, func(r reply) error {
		var item subscription
		if err := json.Unmarshal(r.Body, &item); err != nil {
			return err
		}
		return require(item.ID == first.ID && r.Header.Get("Idempotency-Replayed") == "true", "idempotency replay mismatch")
	})
	if err != nil {
		return err
	}
	if err := s.expectList("Repeated POST did not duplicate data", s.a, 1); err != nil {
		return err
	}
	if err := s.status("Same key with another body rejected", "POST", "", s.a, strings.Replace(body, "400", "401", 1), headers, 409); err != nil {
		return err
	}
	if err := s.status("Missing idempotency key rejected", "POST", "", s.a, body, nil, 400); err != nil {
		return err
	}
	badKey, err := newUUID()
	if err != nil {
		return err
	}
	badHeaders := map[string]string{"Idempotency-Key": badKey}
	if err := s.status("Negative price rejected", "POST", "", s.a, strings.Replace(body, "400", "-1", 1), badHeaders, 400); err != nil {
		return err
	}
	if err := s.status("Reversed dates rejected", "POST", "", s.a, `{"service_name":"Invalid","price":100,"start_date":"09-2025","end_date":"08-2025"}`, badHeaders, 400); err != nil {
		return err
	}
	if err := s.status("User ID in request body rejected", "POST", "", s.a, strings.TrimSuffix(body, "}")+`,"user_id":"`+s.userB+`"}`, badHeaders, 400); err != nil {
		return err
	}
	if err := s.status("Duplicate JSON field rejected", "POST", "", s.a, strings.TrimSuffix(body, "}")+`,"price":500}`, badHeaders, 400); err != nil {
		return err
	}
	if err := s.status("Unknown query field rejected", "GET", "?user_id="+s.userB, s.a, "", nil, 400); err != nil {
		return err
	}
	_, err = s.check("Read subscription by ID", "GET", "/"+first.ID, s.a, "", nil, 200, func(r reply) error {
		var item subscription
		if err := json.Unmarshal(r.Body, &item); err != nil {
			return err
		}
		return require(item.ID == first.ID && item.Price == 400, "wrong record")
	})
	if err != nil {
		return err
	}
	if err := s.status("Other user cannot read this ID", "GET", "/"+first.ID, s.b, "", nil, 404); err != nil {
		return err
	}
	if err := s.status("Other user cannot update this ID", "PATCH", "/"+first.ID, s.b, `{"price":999}`, map[string]string{"If-Match": `"1"`}, 404); err != nil {
		return err
	}
	if err := s.status("Other user cannot delete this ID", "DELETE", "/"+first.ID, s.b, "", map[string]string{"If-Match": `"1"`}, 404); err != nil {
		return err
	}
	if err := s.expectList("Other user cannot list these records", s.b, 0); err != nil {
		return err
	}
	secondKey, err := newUUID()
	if err != nil {
		return err
	}
	var second subscription
	_, err = s.check("Create bounded one-month subscription", "POST", "", s.a, `{"service_name":"Smoke Extra","price":100,"start_date":"08-2025","end_date":"08-2025"}`, map[string]string{"Idempotency-Key": secondKey}, 201, func(r reply) error {
		if err := json.Unmarshal(r.Body, &second); err != nil {
			return err
		}
		return require(second.ID != "" && second.ID != first.ID && second.UserID == s.userA && second.Version == 1, "invalid second subscription")
	})
	if err != nil {
		return err
	}
	if err := s.expectTotal("Monthly overlap: 400*3 + 100 = 1300", 1300); err != nil {
		return err
	}
	var firstPage page
	_, err = s.check("Pagination first page", "GET", "?limit=1", s.a, "", nil, 200, func(r reply) error {
		if err := json.Unmarshal(r.Body, &firstPage); err != nil {
			return err
		}
		return require(len(firstPage.Items) == 1 && firstPage.Next != "", "missing first page/cursor")
	})
	if err != nil {
		return err
	}
	_, err = s.check("Pagination second page", "GET", "?limit=1&cursor="+url.QueryEscape(firstPage.Next), s.a, "", nil, 200, func(r reply) error {
		var p page
		if err := json.Unmarshal(r.Body, &p); err != nil {
			return err
		}
		return require(len(p.Items) == 1 && p.Items[0].ID != firstPage.Items[0].ID && p.Next == "", "duplicate or missing page data")
	})
	if err != nil {
		return err
	}
	if err := s.status("Cursor bound to owner", "GET", "?limit=1&cursor="+url.QueryEscape(firstPage.Next), s.b, "", nil, 400); err != nil {
		return err
	}
	if err := s.status("Tampered cursor rejected", "GET", "?limit=1&cursor="+url.QueryEscape(firstPage.Next+"x"), s.a, "", nil, 400); err != nil {
		return err
	}
	if err := s.status("PATCH requires If-Match", "PATCH", "/"+first.ID, s.a, `{"price":500}`, nil, 428); err != nil {
		return err
	}
	_, err = s.check("PATCH increases version and changes price/end", "PATCH", "/"+first.ID, s.a, `{"price":500,"end_date":"08-2025"}`, map[string]string{"If-Match": `"1"`}, 200, func(r reply) error {
		var item subscription
		if err := json.Unmarshal(r.Body, &item); err != nil {
			return err
		}
		return require(item.Version == 2 && item.Price == 500 && item.EndDate != nil && *item.EndDate == "08-2025" && r.Header.Get("ETag") == `"2"`, "incorrect patch result")
	})
	if err != nil {
		return err
	}
	if err := s.status("Stale PATCH rejected", "PATCH", "/"+first.ID, s.a, `{"price":999}`, map[string]string{"If-Match": `"1"`}, 412); err != nil {
		return err
	}
	if err := s.expectTotal("Updated total: 500*2 + 100 = 1100", 1100); err != nil {
		return err
	}
	if err := s.status("Invalid PATCH rejected", "PATCH", "/"+first.ID, s.a, `{"start_date":"09-2025"}`, map[string]string{"If-Match": `"2"`}, 400); err != nil {
		return err
	}
	_, err = s.check("Clear end_date with null; failed PATCH did not change version", "PATCH", "/"+first.ID, s.a, `{"end_date":null}`, map[string]string{"If-Match": `"2"`}, 200, func(r reply) error {
		var item subscription
		if err := json.Unmarshal(r.Body, &item); err != nil {
			return err
		}
		return require(item.Version == 3 && item.EndDate == nil, "end date was not cleared")
	})
	if err != nil {
		return err
	}
	if err := s.expectTotal("Open-ended total: 500*3 + 100 = 1600", 1600); err != nil {
		return err
	}
	if err := s.status("DELETE requires If-Match", "DELETE", "/"+first.ID, s.a, "", nil, 428); err != nil {
		return err
	}
	if err := s.status("Stale DELETE rejected", "DELETE", "/"+first.ID, s.a, "", map[string]string{"If-Match": `"2"`}, 412); err != nil {
		return err
	}
	if err := s.status("Delete first subscription", "DELETE", "/"+first.ID, s.a, "", map[string]string{"If-Match": `"3"`}, 204); err != nil {
		return err
	}
	if err := s.status("Deleted subscription returns 404", "GET", "/"+first.ID, s.a, "", nil, 404); err != nil {
		return err
	}
	if err := s.status("Delete second subscription", "DELETE", "/"+second.ID, s.a, "", map[string]string{"If-Match": `"1"`}, 204); err != nil {
		return err
	}
	if err := s.expectList("List empty after cleanup", s.a, 0); err != nil {
		return err
	}
	return s.expectTotal("Total zero after cleanup", 0)
}
func (s *suite) cleanup() error {
	// Include unexpected extra rows caused by a failing idempotency/validation check.
	// This is a new random test subject, never an existing real user.
	r, err := s.request("GET", "?limit=100", s.a, "", nil)
	if err != nil {
		return fmt.Errorf("cannot list test user's remaining data: %w", err)
	}
	if r.Code != 200 {
		return fmt.Errorf("cannot list remaining data (HTTP %d)", r.Code)
	}
	var p page
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return err
	}
	if p.Next != "" {
		return fmt.Errorf("unexpected >100 test rows; manual review needed for user %s", s.userA)
	}
	for _, item := range p.Items {
		if item.UserID != s.userA {
			return fmt.Errorf("owner mismatch during cleanup; refusing deletion")
		}
		r, err := s.request("DELETE", "/"+item.ID, s.a, "", map[string]string{"If-Match": fmt.Sprintf(`"%d"`, item.Version)})
		if err != nil {
			return err
		}
		if r.Code != 204 && r.Code != 404 {
			return fmt.Errorf("cleanup returned HTTP %d for test row %s", r.Code, item.ID)
		}
	}
	return nil
}
