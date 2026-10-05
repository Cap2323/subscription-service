package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/subscriptions/internal/auth"
	"example.com/subscriptions/internal/domain"
	"example.com/subscriptions/internal/observability"
	"github.com/google/uuid"
)

type fakeAuth struct{ user uuid.UUID }

func (f fakeAuth) Verify(s string) (auth.Principal, error) {
	if s == "good" {
		return auth.Principal{User: f.user, Read: true, Write: true}, nil
	}
	if s == "read" {
		return auth.Principal{User: f.user, Read: true}, nil
	}
	return auth.Principal{}, errors.New("bad token")
}

type fakeRepo struct {
	Repository
	calls   int
	gotUser uuid.UUID
}

func (f *fakeRepo) Create(_ context.Context, u uuid.UUID, _ string, r domain.Create) (domain.Subscription, bool, error) {
	f.calls++
	f.gotUser = u
	return domain.Subscription{}, false, r.Validate()
}
func (f *fakeRepo) List(_ context.Context, u uuid.UUID, r domain.List) ([]domain.Subscription, error) {
	f.calls++
	f.gotUser = u
	if r.Limit < 1 || r.Limit > 100 {
		return nil, domain.Invalid("bad limit")
	}
	return []domain.Subscription{}, nil
}
func setup(t *testing.T) (http.Handler, *fakeRepo, uuid.UUID) {
	t.Helper()
	user := uuid.New()
	repo := &fakeRepo{}
	a := &API{Repo: repo, Auth: fakeAuth{user}, Cursor: CursorCodec{Key: []byte(strings.Repeat("x", 32))}, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Metrics: observability.New(), MaxConcurrent: 8, RPS: 10000}
	return a.Handler(), repo, user
}
func call(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "1234567890123456")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthAndOwnershipBoundary(t *testing.T) {
	h, repo, user := setup(t)
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		w := call(h, method, "/api/v1/subscriptions", `{}`, "")
		if w.Code != 401 {
			t.Fatal(method, w.Code)
		}
	}
	if w := call(h, "POST", "/api/v1/subscriptions", `{}`, "read"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := call(h, "GET", "/api/v1/subscriptions?user_id="+uuid.NewString(), "", "good"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call(h, "GET", "/api/v1/subscriptions", "", "good"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if repo.calls != 1 || repo.gotUser != user {
		t.Fatal("identity did not come exclusively from verified JWT")
	}
}
func TestRejectMalformedBodies(t *testing.T) {
	h, repo, _ := setup(t)
	for _, body := range []string{`[]`, `null`, `{} {}`, `{"price":1,"price":2}`, `{"user_id":"x"}`, `{"price":null}`, `{"price":`, "{\"service_name\":\"" + string([]byte{0xff}) + "\"}"} {
		if w := call(h, "POST", "/api/v1/subscriptions", body, "good"); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	if w := call(h, "POST", "/api/v1/subscriptions", `{"service_name":"`+strings.Repeat("x", 16384)+`"}`, "good"); w.Code != 413 {
		t.Fatal(w.Code)
	}
	if repo.calls != 0 {
		t.Fatal("bad JSON reached repository")
	}
}
func TestMissingVersion(t *testing.T) {
	h, _, _ := setup(t)
	for _, method := range []string{"PATCH", "DELETE"} {
		if w := call(h, method, "/api/v1/subscriptions/"+uuid.NewString(), `{}`, "good"); w.Code != 428 {
			t.Fatal(method, w.Code)
		}
	}
}
func TestCursorBindingAndTampering(t *testing.T) {
	c := CursorCodec{Key: []byte(strings.Repeat("k", 32))}
	user := uuid.New()
	value := Cursor{User: user, Name: "A", Time: time.Now().UTC(), ID: uuid.New(), Exp: time.Now().Add(time.Hour).Unix()}
	token := c.Encode(value)
	if _, e := c.Decode(token, user, "A"); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Decode(token, uuid.New(), "A"); e == nil {
		t.Fatal("cursor usable by another user")
	}
	if _, e := c.Decode(token, user, "B"); e == nil {
		t.Fatal("cursor usable with another filter")
	}
	if _, e := c.Decode(token+"x", user, "A"); e == nil {
		t.Fatal("tampering accepted")
	}
	value.Exp = time.Now().Add(-time.Hour).Unix()
	if _, e := c.Decode(c.Encode(value), user, "A"); e == nil {
		t.Fatal("expired cursor accepted")
	}
}
