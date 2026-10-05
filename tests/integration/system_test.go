//go:build integration

package integration

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/subscriptions/internal/config"
	"example.com/subscriptions/internal/domain"
	"example.com/subscriptions/internal/store"
	"example.com/subscriptions/internal/topology"
	"example.com/subscriptions/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests deliberately reset only databases named subscriptions_test.
// They fail closed unless explicit test credentials and reset consent are supplied.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	if os.Getenv("TEST_ALLOW_RESET") != "yes" {
		t.Fatal("set TEST_ALLOW_RESET=yes and TEST_SHARD_* DSNs to isolated subscriptions_test databases")
	}
	c := config.Config{Profile: "development", PoolMax: 12, Buckets: 32, Epoch: 1}
	for _, letter := range []string{"A", "B", "C"} {
		s := config.Shard{ID: "shard-" + strings.ToLower(letter), DSNEnv: "TEST_SHARD_" + letter + "_DSN", AdminDSNEnv: "TEST_SHARD_" + letter + "_ADMIN_DSN"}
		for _, name := range []string{s.DSNEnv, s.AdminDSNEnv} {
			v, e := config.Secret(name)
			if e != nil {
				t.Fatal(e)
			}
			u, e := url.Parse(v)
			if e != nil || u.Path != "/subscriptions_test" {
				t.Fatalf("%s must point to subscriptions_test", name)
			}
		}
		c.Shards = append(c.Shards, s)
	}
	return c
}
func open(t *testing.T, ctx context.Context, c config.Config, admin bool) *store.Store {
	t.Helper()
	s, e := store.Open(ctx, c, admin)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}
func month(t *testing.T, v string) domain.Month {
	t.Helper()
	m, e := domain.ParseMonth(v)
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func TestShardedSystem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	allConfig := testConfig(t)
	all := open(t, ctx, allConfig, true)
	for _, pool := range all.Pools {
		_, e := pool.Exec(ctx, `DROP FUNCTION IF EXISTS public.check_routing(TEXT,TEXT);DROP TABLE IF EXISTS public.subscriptions,public.user_usage,public.idempotency,public.routing_guard,public.schema_migrations CASCADE;`)
		if e != nil {
			t.Fatal(e)
		}
		if e := migrations.Apply(ctx, pool); e != nil {
			t.Fatal(e)
		}
		if e := migrations.Apply(ctx, pool); e != nil {
			t.Fatal("migration not idempotent", e)
		}
	}
	oldConfig := allConfig
	oldConfig.Shards = append([]config.Shard(nil), allConfig.Shards[:2]...)
	admin := open(t, ctx, oldConfig, true)
	if e := topology.Initialize(ctx, admin); e != nil {
		t.Fatal(e)
	}
	runtime := open(t, ctx, oldConfig, false)
	if e := runtime.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	user := uuid.New()
	create := domain.Create{ServiceName: "A", Price: 400, StartDate: month(t, "05-2025")}
	one, replay, e := runtime.Create(ctx, user, "create-key-00000001", create)
	if e != nil || replay {
		t.Fatal(e)
	}
	again, replay, e := runtime.Create(ctx, user, "create-key-00000001", create)
	if e != nil || !replay || one.ID != again.ID {
		t.Fatal("idempotency", e)
	}
	different := create
	different.Price = 500
	if _, _, e := runtime.Create(ctx, user, "create-key-00000001", different); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("key reuse", e)
	}
	if _, e := runtime.Get(ctx, uuid.New(), one.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("owner isolation", e)
	}
	end := month(t, "08-2025")
	if _, _, e := runtime.Create(ctx, user, "create-key-00000002", domain.Create{ServiceName: "A", Price: 100, StartDate: end, EndDate: &end}); e != nil {
		t.Fatal(e)
	}
	total, e := runtime.Total(ctx, user, domain.Period{From: month(t, "07-2025"), To: month(t, "09-2025"), Name: "A"})
	if e != nil || total != 1300 {
		t.Fatal("monthly total", total, e)
	}
	price := int64(500)
	patched, e := runtime.Patch(ctx, user, one.ID, one.Version, domain.Patch{Price: &price})
	if e != nil || patched.Version != 2 {
		t.Fatal("patch", e)
	}
	if _, e := runtime.Patch(ctx, user, one.ID, one.Version, domain.Patch{Price: &price}); !errors.Is(e, domain.ErrVersion) {
		t.Fatal("stale update accepted", e)
	}
	if e := runtime.Delete(ctx, user, one.ID, one.Version); !errors.Is(e, domain.ErrVersion) {
		t.Fatal("stale deletion accepted", e)
	}
	page, e := runtime.List(ctx, user, domain.List{Limit: 1})
	if e != nil || len(page) != 2 {
		t.Fatal("limit+1", e)
	}
	next, e := runtime.List(ctx, user, domain.List{Limit: 1, AfterTime: page[0].CreatedAt, AfterID: page[0].ID})
	if e != nil || len(next) != 1 || next[0].ID == page[0].ID {
		t.Fatal("keyset pagination", e)
	}

	t.Run("concurrent idempotency", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan uuid.UUID, 12)
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, _, e := runtime.Create(ctx, user, "concurrent-key-001", create)
				if e != nil {
					errs <- e
				} else {
					results <- s.ID
				}
			}()
		}
		wg.Wait()
		close(errs)
		close(results)
		for e := range errs {
			t.Error(e)
		}
		var id uuid.UUID
		for result := range results {
			if id == uuid.Nil {
				id = result
			}
			if result != id {
				t.Fatal("duplicate rows")
			}
		}
	})
	t.Run("RLS blocks missing/wrong tenant", func(t *testing.T) {
		_, shard := runtime.Ring.Route(user)
		pool := runtime.Pools[shard]
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		var count int
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM public.subscriptions`).Scan(&count); e != nil || count != 0 {
			t.Fatal("RLS without context", count, e)
		}
		if _, e := tx.Exec(ctx, `SELECT set_config('app.user_id',$1,TRUE)`, uuid.NewString()); e != nil {
			t.Fatal(e)
		}
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM public.subscriptions WHERE user_id=$1`, user).Scan(&count); e != nil || count != 0 {
			t.Fatal("RLS different user", count, e)
		}
		if _, e := tx.Exec(ctx, `UPDATE public.routing_guard SET frozen=TRUE`); e == nil {
			t.Fatal("runtime can edit routing fence")
		}
	})

	nextConfig := allConfig
	nextConfig.Epoch = 2
	nextRing, e := nextConfig.Ring()
	if e != nil {
		t.Fatal(e)
	}
	var moving uuid.UUID
	var bucket int
	var source, target string
	for i := 0; i < 10000; i++ {
		u := uuid.New()
		b, a := runtime.Ring.Route(u)
		_, z := nextRing.Route(u)
		if a != z {
			moving = u
			bucket = b
			source = a
			target = z
			break
		}
	}
	if moving == uuid.Nil {
		t.Fatal("no moving user found")
	}
	moved, _, e := runtime.Create(ctx, moving, "moving-key-000001", create)
	if e != nil {
		t.Fatal(e)
	}
	rb := topology.Rebalancer{Old: runtime.Ring, Next: nextRing, Pools: all.Pools}
	if e := rb.Run(ctx, "freeze"); e != nil {
		t.Fatal(e)
	}
	if _, e := runtime.Get(ctx, moving, moved.ID); !errors.Is(e, domain.ErrUnavailable) {
		t.Fatal("old routing accepted while frozen", e)
	}
	if _, e := all.Pools[source].Exec(ctx, `UPDATE public.routing_guard SET shard_id='wrong-shard' WHERE singleton`); e != nil {
		t.Fatal(e)
	}
	if e := rb.Run(ctx, "copy"); e == nil {
		t.Fatal("copy accepted a wrong physical shard identity")
	}
	if _, e := all.Pools[source].Exec(ctx, `UPDATE public.routing_guard SET shard_id=$1 WHERE singleton`, source); e != nil {
		t.Fatal(e)
	}
	if e := rb.Run(ctx, "copy"); e != nil {
		t.Fatal(e)
	}
	if e := rb.Run(ctx, "copy"); e != nil {
		t.Fatal("copy not resumable", e)
	}
	if e := rb.Run(ctx, "activate"); e != nil {
		t.Fatal(e)
	}
	newRuntime := open(t, ctx, nextConfig, false)
	if e := newRuntime.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := newRuntime.Get(ctx, moving, moved.ID)
	if e != nil || got.ID != moved.ID || got.Price != 400 {
		t.Fatal("data lost after move", got, e)
	}
	replayed, replay, e := newRuntime.Create(ctx, moving, "moving-key-000001", create)
	if e != nil || !replay || replayed.ID != moved.ID {
		t.Fatal("idempotency lost after move", e)
	}
	if _, e := runtime.Get(ctx, moving, moved.ID); !errors.Is(e, domain.ErrUnavailable) {
		t.Fatal("stale topology accepted after switch", e)
	}
	if _, e := newRuntime.Patch(ctx, moving, moved.ID, got.Version, domain.Patch{Price: &price}); e != nil {
		t.Fatal(e)
	}
	if e := rb.Run(ctx, "copy"); e == nil {
		t.Fatal("copy allowed after activation; could overwrite new writes")
	}
	if e := rb.Run(ctx, "activate"); e != nil {
		t.Fatal("activation not resumable", e)
	}
	got, e = newRuntime.Get(ctx, moving, moved.ID)
	if e != nil || got.Price != 500 {
		t.Fatal("activation replay rewrote data", e)
	}
	newAdmin := open(t, ctx, nextConfig, true)
	if e := topology.Acknowledge(ctx, newAdmin); e != nil {
		t.Fatal(e)
	}
	if e := topology.GCStale(ctx, newAdmin); e != nil {
		t.Fatal(e)
	}
	count := func(p *pgxpool.Pool) int {
		var n int
		if e := p.QueryRow(ctx, `SELECT count(*) FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2`, bucket, moving).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if count(all.Pools[source]) != 0 || count(all.Pools[target]) != 1 {
		t.Fatal("stale-copy garbage collection affected owner")
	}
	if _, e := all.Pools[target].Exec(ctx, `UPDATE public.idempotency SET expires_at=clock_timestamp()-interval '1 hour' WHERE user_id=$1`, moving); e != nil {
		t.Fatal(e)
	}
	if e := topology.Cleanup(ctx, newAdmin); e != nil {
		t.Fatal(e)
	}
	if e := newRuntime.Delete(ctx, moving, moved.ID, got.Version); e != nil {
		t.Fatal(e)
	}
}
