package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"example.com/subscriptions/internal/auth"
	"example.com/subscriptions/internal/domain"
	"example.com/subscriptions/internal/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/time/rate"
)

type Repository interface {
	Create(context.Context, uuid.UUID, string, domain.Create) (domain.Subscription, bool, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (domain.Subscription, error)
	List(context.Context, uuid.UUID, domain.List) ([]domain.Subscription, error)
	Patch(context.Context, uuid.UUID, uuid.UUID, int64, domain.Patch) (domain.Subscription, error)
	Delete(context.Context, uuid.UUID, uuid.UUID, int64) error
	Total(context.Context, uuid.UUID, domain.Period) (int64, error)
}
type TokenVerifier interface {
	Verify(string) (auth.Principal, error)
}
type API struct {
	Repo               Repository
	Auth               TokenVerifier
	Cursor             CursorCodec
	Log                *slog.Logger
	Metrics            *observability.Metrics
	MaxConcurrent, RPS int
}
type contextKey int

const principalKey contextKey = 0

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/subscriptions", a.create)
	mux.HandleFunc("GET /api/v1/subscriptions", a.list)
	mux.HandleFunc("GET /api/v1/subscriptions/total", a.total)
	mux.HandleFunc("GET /api/v1/subscriptions/{id}", a.get)
	mux.HandleFunc("PATCH /api/v1/subscriptions/{id}", a.patch)
	mux.HandleFunc("DELETE /api/v1/subscriptions/{id}", a.delete)
	slots := make(chan struct{}, a.MaxConcurrent)
	limiter := rate.NewLimiter(rate.Limit(a.RPS), a.RPS)
	var sampledReads atomic.Uint64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		requestID := uuid.NewString()
		rw.Header().Set("X-Request-ID", requestID)
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Cache-Control", "no-store")
		_, pattern := mux.Handler(r)
		route := pattern
		if route == "" {
			route = "unmatched"
		}
		method := r.Method
		switch method {
		case "GET", "POST", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			method = "OTHER"
		}
		a.Metrics.Inflight.Inc()
		defer func() {
			if recovered := recover(); recovered != nil {
				a.Log.Error("panic", "request_id", requestID, "type", fmt.Sprintf("%T", recovered))
				if !rw.wrote {
					writeError(rw, 500, "internal_error", "internal server error")
				}
			}
			a.Metrics.Inflight.Dec()
			a.Metrics.Requests.WithLabelValues(route, method, strconv.Itoa(rw.status)).Inc()
			a.Metrics.Duration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
			// Preserve all errors and mutations, but sample ordinary successful reads.
			if rw.status >= 400 || method != "GET" || time.Since(start) >= time.Second || sampledReads.Add(1)%100 == 0 {
				a.Log.Info("request", "request_id", requestID, "route", route, "method", method, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		if !limiter.Allow() {
			rw.Header().Set("Retry-After", "1")
			writeError(rw, 429, "rate_limited", "request rate exceeded")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			rw.Header().Set("Retry-After", "1")
			writeError(rw, 503, "busy", "server busy")
			return
		}
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			unauthorized(rw)
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(rw)
			return
		}
		p, err := a.Auth.Verify(parts[1])
		if err != nil {
			unauthorized(rw)
			return
		}
		read := r.Method == "GET" || r.Method == "HEAD"
		if (read && !p.Read) || (!read && !p.Write) {
			writeError(rw, 403, "forbidden", "scope required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		ctx = context.WithValue(ctx, principalKey, p)
		mux.ServeHTTP(rw, r.WithContext(ctx))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func user(r *http.Request) uuid.UUID                { return r.Context().Value(principalKey).(auth.Principal).User }
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, 401, "unauthorized", "valid bearer token required")
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message, "request_id": w.Header().Get("X-Request-ID")})
}
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var validation *domain.ValidationError
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &validation):
		writeError(w, 400, "validation", validation.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, 404, "not_found", "subscription not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, 409, "idempotency_conflict", err.Error())
	case errors.Is(err, domain.ErrVersion):
		writeError(w, 412, "version_conflict", err.Error())
	case errors.Is(err, domain.ErrQuota):
		writeError(w, 409, "quota_exceeded", err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		writeError(w, 503, "unavailable", "shard unavailable or topology changing")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, 504, "timeout", "request timed out")
	case errors.Is(err, context.Canceled):
		return
	case errors.As(err, &pg) && (pg.Code == "57014" || pg.Code == "55P03" || pg.Code == "40001" || pg.Code == "40P01"):
		writeError(w, 503, "retryable", "database busy; retry with the same idempotency key or version")
	default:
		// Do not log database detail/query/body, which may contain user data.
		code := ""
		if errors.As(err, &pg) {
			code = pg.Code
		}
		a.Log.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "error_type", fmt.Sprintf("%T", err), "sqlstate", code)
		writeError(w, 500, "internal_error", "internal server error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		writeError(w, 415, "content_type", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			writeError(w, 413, "body_too_large", "maximum JSON size is 16 KiB")
		} else {
			writeError(w, 400, "json", "cannot read JSON")
		}
		return false
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' || !utf8.Valid(b) {
		writeError(w, 400, "json", "expected a UTF-8 JSON object")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	_, err = d.Token()
	if err != nil {
		writeError(w, 400, "json", "invalid JSON")
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			writeError(w, 400, "json", "invalid JSON")
			return false
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			writeError(w, 400, "json", "duplicate JSON field")
			return false
		}
		seen[name] = true
		switch name {
		case "service_name", "price", "start_date", "end_date":
		default:
			writeError(w, 400, "json", "unknown JSON field")
			return false
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			writeError(w, 400, "json", "invalid JSON")
			return false
		}
		if name != "end_date" && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			writeError(w, 400, "json", "only end_date may be null")
			return false
		}
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		writeError(w, 400, "json", "invalid fields, types or date format")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "json", "expected exactly one JSON object")
		return false
	}
	return true
}
func query(w http.ResponseWriter, r *http.Request, names ...string) (url.Values, bool) {
	v, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, 400, "query", "invalid query")
		return nil, false
	}
	allowed := map[string]bool{}
	for _, n := range names {
		allowed[n] = true
	}
	for k, a := range v {
		if !allowed[k] || len(a) != 1 || a[0] == "" {
			writeError(w, 400, "query", "unknown, repeated or empty query parameter")
			return nil, false
		}
	}
	return v, true
}
func id(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	v, e := uuid.Parse(r.PathValue("id"))
	if e != nil || v == uuid.Nil {
		writeError(w, 400, "id", "invalid UUID")
		return uuid.Nil, false
	}
	return v, true
}
func version(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if len(r.Header.Values("If-Match")) != 1 || r.Header.Get("If-Match") == "" {
		writeError(w, 428, "precondition_required", "If-Match with the current ETag is required")
		return 0, false
	}
	v := r.Header.Get("If-Match")
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		writeError(w, 400, "etag", "expected a quoted integer ETag")
		return 0, false
	}
	n, e := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	if e != nil || n < 1 {
		writeError(w, 400, "etag", "invalid ETag")
		return 0, false
	}
	return n, true
}
func etag(w http.ResponseWriter, v int64) { w.Header().Set("ETag", fmt.Sprintf(`"%d"`, v)) }

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	if _, ok := query(w, r); !ok {
		return
	}
	var req domain.Create
	if !decode(w, r, &req) {
		return
	}
	if len(r.Header.Values("Idempotency-Key")) != 1 {
		writeError(w, 400, "idempotency_key", "one Idempotency-Key is required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	for _, c := range key {
		if c < 33 || c > 126 {
			writeError(w, 400, "idempotency_key", "key must use printable ASCII without spaces")
			return
		}
	}
	s, replay, err := a.Repo.Create(r.Context(), user(r), key, req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	etag(w, s.Version)
	w.Header().Set("Location", "/api/v1/subscriptions/"+s.ID.String())
	if replay {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, 201, s)
}
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := query(w, r); !ok {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	s, err := a.Repo.Get(r.Context(), user(r), v)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	etag(w, s.Version)
	writeJSON(w, 200, s)
}
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	v, ok := query(w, r, "limit", "cursor", "service_name")
	if !ok {
		return
	}
	req := domain.List{Limit: 50, Name: v.Get("service_name")}
	if raw := v.Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil {
			writeError(w, 400, "limit", "limit must be 1..100")
			return
		}
		req.Limit = n
	}
	if raw := v.Get("cursor"); raw != "" {
		cur, e := a.Cursor.Decode(raw, user(r), req.Name)
		if e != nil {
			writeError(w, 400, "cursor", e.Error())
			return
		}
		req.AfterTime = cur.Time
		req.AfterID = cur.ID
	}
	items, err := a.Repo.List(r.Context(), user(r), req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	next := ""
	if len(items) > req.Limit {
		items = items[:req.Limit]
		last := items[len(items)-1]
		next = a.Cursor.Encode(Cursor{user(r), req.Name, last.CreatedAt, last.ID, time.Now().Add(24 * time.Hour).Unix()})
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *API) patch(w http.ResponseWriter, r *http.Request) {
	if _, ok := query(w, r); !ok {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	ver, ok := version(w, r)
	if !ok {
		return
	}
	var req domain.Patch
	if !decode(w, r, &req) {
		return
	}
	s, e := a.Repo.Patch(r.Context(), user(r), v, ver, req)
	if e != nil {
		a.fail(w, r, e)
		return
	}
	etag(w, s.Version)
	writeJSON(w, 200, s)
}
func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	if _, ok := query(w, r); !ok {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	ver, ok := version(w, r)
	if !ok {
		return
	}
	if e := a.Repo.Delete(r.Context(), user(r), v, ver); e != nil {
		a.fail(w, r, e)
		return
	}
	w.WriteHeader(204)
}
func (a *API) total(w http.ResponseWriter, r *http.Request) {
	v, ok := query(w, r, "from", "to", "service_name")
	if !ok {
		return
	}
	from, e := domain.ParseMonth(v.Get("from"))
	if e != nil {
		writeError(w, 400, "from", "from is required in MM-YYYY format")
		return
	}
	to, e := domain.ParseMonth(v.Get("to"))
	if e != nil {
		writeError(w, 400, "to", "to is required in MM-YYYY format")
		return
	}
	sum, e := a.Repo.Total(r.Context(), user(r), domain.Period{From: from, To: to, Name: v.Get("service_name")})
	if e != nil {
		a.fail(w, r, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"total": sum})
}
