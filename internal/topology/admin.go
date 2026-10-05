// Package topology contains privileged, offline operations. Never expose these over HTTP.
package topology

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"example.com/subscriptions/internal/hashring"
	"example.com/subscriptions/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Guard struct {
	Shard, Fingerprint string
	Epoch              int64
	Frozen             bool
	Pending            string
	Copied             bool
}
type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func guard(ctx context.Context, q queryer) (Guard, error) {
	var g Guard
	e := q.QueryRow(ctx, `SELECT shard_id,fingerprint,epoch,frozen,pending_fingerprint,copy_complete FROM public.routing_guard WHERE singleton`).Scan(&g.Shard, &g.Fingerprint, &g.Epoch, &g.Frozen, &g.Pending, &g.Copied)
	return g, e
}

// lockAll serializes administrative operations across all participating shards.
// Sorted order prevents operators taking distributed locks in opposite order.
func lockAll(ctx context.Context, pools map[string]*pgxpool.Pool) (func(), error) {
	ids := make([]string, 0, len(pools))
	for id := range pools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	conns := make([]*pgx.Conn, 0, len(ids))
	closeAll := func() {
		for i := len(conns) - 1; i >= 0; i-- {
			conns[i].Close(context.Background())
		}
	}
	for _, id := range ids {
		c, e := pgx.ConnectConfig(ctx, pools[id].Config().ConnConfig.Copy())
		if e != nil {
			closeAll()
			return nil, e
		}
		conns = append(conns, c)
		var ok bool
		if e := c.QueryRow(ctx, `SELECT pg_try_advisory_lock(764302199)`).Scan(&ok); e != nil || !ok {
			closeAll()
			return nil, fmt.Errorf("another admin operation holds shard %s", id)
		}
	}
	return closeAll, nil
}

func Initialize(ctx context.Context, s *store.Store) error {
	unlock, e := lockAll(ctx, s.Pools)
	if e != nil {
		return e
	}
	defer unlock()
	for _, id := range s.Ring.Spec.Shards {
		g, e := guard(ctx, s.Pools[id])
		if e != nil {
			return e
		}
		if g.Fingerprint != "" && (g.Shard != id || g.Fingerprint != s.Ring.Fingerprint || g.Frozen || g.Pending != "") {
			return fmt.Errorf("%s already initialized for a different or frozen topology", id)
		}
		if g.Fingerprint == "" {
			if g.Pending != "" || g.Shard != "" {
				return fmt.Errorf("shard %s participates in an unfinished topology operation", id)
			}
			var exists bool
			e := s.Pools[id].QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.subscriptions UNION ALL SELECT 1 FROM public.idempotency UNION ALL SELECT 1 FROM public.user_usage)`).Scan(&exists)
			if e != nil {
				return e
			}
			if exists {
				return fmt.Errorf("refusing to initialize nonempty shard %s", id)
			}
		}
	}
	for _, id := range s.Ring.Spec.Shards {
		_, e := s.Pools[id].Exec(ctx, `UPDATE public.routing_guard SET shard_id=$1,fingerprint=$2,epoch=$3,frozen=FALSE WHERE singleton`, id, s.Ring.Fingerprint, s.Ring.Spec.Epoch)
		if e != nil {
			return e
		}
	}
	return nil
}

type Rebalancer struct {
	Old, Next *hashring.Ring
	Pools     map[string]*pgxpool.Pool
}

func (r Rebalancer) ids() []string {
	ids := make([]string, 0, len(r.Pools))
	for id := range r.Pools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func contains(items []string, s string) bool {
	for _, i := range items {
		if i == s {
			return true
		}
	}
	return false
}

func (r Rebalancer) Run(ctx context.Context, phase string) error {
	if _, e := hashring.Plan(r.Old, r.Next); e != nil {
		return e
	}
	unlock, e := lockAll(ctx, r.Pools)
	if e != nil {
		return e
	}
	defer unlock()
	switch phase {
	case "freeze":
		return r.freeze(ctx)
	case "copy":
		return r.copy(ctx)
	case "activate":
		return r.activate(ctx)
	default:
		return fmt.Errorf("phase must be freeze, copy or activate")
	}
}
func (r Rebalancer) freeze(ctx context.Context) error {
	// Preflight all shards before freezing any. Resume is allowed only before activation.
	for _, id := range r.ids() {
		g, e := guard(ctx, r.Pools[id])
		if e != nil {
			return e
		}
		if g.Fingerprint == r.Next.Fingerprint {
			return fmt.Errorf("activation has begun; only resume activate, never copy again")
		}
		if g.Pending != "" && g.Pending != r.Next.Fingerprint {
			return fmt.Errorf("different rebalance is pending on %s", id)
		}
		if contains(r.Old.Spec.Shards, id) {
			if g.Shard != id || g.Fingerprint != r.Old.Fingerprint {
				return fmt.Errorf("unexpected old topology on %s", id)
			}
		} else {
			if g.Fingerprint != "" || g.Shard != "" && g.Shard != id {
				return fmt.Errorf("new shard %s is not empty/uninitialized", id)
			}
			if g.Pending == "" {
				var exists bool
				if e := r.Pools[id].QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.subscriptions UNION ALL SELECT 1 FROM public.idempotency UNION ALL SELECT 1 FROM public.user_usage)`).Scan(&exists); e != nil {
					return e
				}
				if exists {
					return fmt.Errorf("new shard %s contains data", id)
				}
			}
		}
	}
	for _, id := range r.ids() {
		// UPDATE waits for every in-flight transaction holding check_routing's FOR SHARE.
		_, e := r.Pools[id].Exec(ctx, `UPDATE public.routing_guard SET shard_id=$1,frozen=TRUE,pending_fingerprint=$2,copy_complete=FALSE WHERE singleton`, id, r.Next.Fingerprint)
		if e != nil {
			return e
		}
	}
	return nil
}

func (r Rebalancer) copy(ctx context.Context) error {
	for _, id := range r.ids() {
		g, e := guard(ctx, r.Pools[id])
		if e != nil {
			return e
		}
		if g.Shard != id || !g.Frozen || g.Pending != r.Next.Fingerprint || g.Fingerprint == r.Next.Fingerprint {
			return fmt.Errorf("all shards must be frozen for this plan; activation must not have begun")
		}
		if contains(r.Old.Spec.Shards, id) && g.Fingerprint != r.Old.Fingerprint {
			return fmt.Errorf("old shard identity/topology mismatch on %s", id)
		}
		if !contains(r.Old.Spec.Shards, id) && g.Fingerprint != "" {
			return fmt.Errorf("new shard %s was initialized for another topology", id)
		}
	}
	moves, _ := hashring.Plan(r.Old, r.Next)
	for _, move := range moves {
		if e := copyBucket(ctx, r.Pools[move.From], r.Pools[move.To], move.Bucket); e != nil {
			return fmt.Errorf("copy bucket %d (%s -> %s): %w", move.Bucket, move.From, move.To, e)
		}
	}
	for _, id := range r.ids() {
		if _, e := r.Pools[id].Exec(ctx, `UPDATE public.routing_guard SET copy_complete=TRUE WHERE singleton`); e != nil {
			return e
		}
	}
	return nil
}

func (r Rebalancer) activate(ctx context.Context) error {
	for _, id := range r.ids() {
		g, e := guard(ctx, r.Pools[id])
		if e != nil {
			return e
		}
		if g.Shard != id || !g.Copied || g.Pending != r.Next.Fingerprint || (g.Fingerprint != r.Old.Fingerprint && g.Fingerprint != "" && g.Fingerprint != r.Next.Fingerprint) {
			return fmt.Errorf("copy is incomplete or topology differs on %s", id)
		}
		if !g.Frozen && g.Fingerprint != r.Next.Fingerprint {
			return fmt.Errorf("old topology is still serving on %s", id)
		}
	}
	// Fence old binaries on every shard before enabling any new routing.
	for _, id := range r.ids() {
		if _, e := r.Pools[id].Exec(ctx, `UPDATE public.routing_guard SET fingerprint=$1,epoch=$2 WHERE singleton`, r.Next.Fingerprint, r.Next.Spec.Epoch); e != nil {
			return e
		}
	}
	for _, id := range r.Next.Spec.Shards {
		if _, e := r.Pools[id].Exec(ctx, `UPDATE public.routing_guard SET frozen=FALSE WHERE singleton`); e != nil {
			return e
		}
	}
	// Keep pending/copy_complete as a durable receipt so activation can be resumed.
	return nil
}

// A separate acknowledgement releases the completed plan for the next rebalance.
// It never unfreezes or deletes data and is safe to repeat.
func Acknowledge(ctx context.Context, s *store.Store) error {
	unlock, e := lockAll(ctx, s.Pools)
	if e != nil {
		return e
	}
	defer unlock()
	if e := s.Ready(ctx); e != nil {
		return e
	}
	for _, id := range s.Ring.Spec.Shards {
		g, e := guard(ctx, s.Pools[id])
		if e != nil {
			return e
		}
		if g.Pending != "" && g.Pending != s.Ring.Fingerprint {
			return fmt.Errorf("another plan is pending")
		}
	}
	for _, id := range s.Ring.Spec.Shards {
		if _, e := s.Pools[id].Exec(ctx, `UPDATE public.routing_guard SET pending_fingerprint='',copy_complete=FALSE WHERE singleton`); e != nil {
			return e
		}
	}
	return nil
}

var tables = []struct{ name, order string }{{"subscriptions", "user_id,id"}, {"user_usage", "user_id"}, {"idempotency", "user_id,key_hash"}}

func copyBucket(ctx context.Context, source, target *pgxpool.Pool, bucket int) error {
	tx, e := target.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	for _, table := range tables {
		// Table identifiers come only from the fixed allowlist above.
		if _, e := tx.Exec(ctx, `DELETE FROM public.`+table.name+` WHERE bucket_id=$1`, bucket); e != nil {
			return e
		}
		rows, e := source.Query(ctx, `SELECT to_jsonb(t)::text FROM public.`+table.name+` t WHERE bucket_id=$1 ORDER BY `+table.order, bucket)
		if e != nil {
			return e
		}
		expected := sha256.New()
		var expectedCount int64
		batch := &pgx.Batch{}
		flush := func() error {
			if batch.Len() == 0 {
				return nil
			}
			results := tx.SendBatch(ctx, batch)
			e := results.Close()
			batch = &pgx.Batch{}
			return e
		}
		for rows.Next() {
			var jsonRow string
			if e := rows.Scan(&jsonRow); e != nil {
				rows.Close()
				return e
			}
			writeHash(expected, []byte(jsonRow))
			expectedCount++
			batch.Queue(`INSERT INTO public.`+table.name+` SELECT * FROM jsonb_populate_record(NULL::public.`+table.name+`,$1::jsonb)`, jsonRow)
			if batch.Len() == 500 {
				if e := flush(); e != nil {
					rows.Close()
					return e
				}
			}
		}
		rows.Close()
		if e := rows.Err(); e != nil {
			return e
		}
		if e := flush(); e != nil {
			return e
		}
		check, e := tx.Query(ctx, `SELECT to_jsonb(t)::text FROM public.`+table.name+` t WHERE bucket_id=$1 ORDER BY `+table.order, bucket)
		if e != nil {
			return e
		}
		actual := sha256.New()
		var actualCount int64
		for check.Next() {
			var row string
			if e := check.Scan(&row); e != nil {
				check.Close()
				return e
			}
			writeHash(actual, []byte(row))
			actualCount++
		}
		check.Close()
		if e := check.Err(); e != nil {
			return e
		}
		if expectedCount != actualCount || hex.EncodeToString(expected.Sum(nil)) != hex.EncodeToString(actual.Sum(nil)) {
			return fmt.Errorf("row count/checksum mismatch for %s", table.name)
		}
	}
	return tx.Commit(ctx)
}

type hashWriter interface{ Write([]byte) (int, error) }

func writeHash(h hashWriter, b []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(b)))
	h.Write(size[:])
	h.Write(b)
}

func Cleanup(ctx context.Context, s *store.Store) error {
	for _, id := range s.Ring.Spec.Shards {
		for batch := 0; batch < 200; batch++ {
			tx, e := s.Pools[id].Begin(ctx)
			if e != nil {
				return e
			}
			var ok bool
			e = tx.QueryRow(ctx, `SELECT public.check_routing($1,$2)`, id, s.Ring.Fingerprint).Scan(&ok)
			if e != nil || !ok {
				tx.Rollback(ctx)
				return fmt.Errorf("cleanup refused on frozen/stale shard %s", id)
			}
			tag, e := tx.Exec(ctx, `DELETE FROM public.idempotency WHERE ctid IN (SELECT ctid FROM public.idempotency WHERE expires_at<clock_timestamp() ORDER BY expires_at LIMIT 5000 FOR UPDATE SKIP LOCKED)`)
			if e != nil {
				tx.Rollback(ctx)
				return e
			}
			if e := tx.Commit(ctx); e != nil {
				return e
			}
			if tag.RowsAffected() < 5000 {
				break
			}
		}
	}
	return nil
}

func GCStale(ctx context.Context, s *store.Store) error {
	unlock, e := lockAll(ctx, s.Pools)
	if e != nil {
		return e
	}
	defer unlock()
	if e := s.Ready(ctx); e != nil {
		return e
	}
	for _, id := range s.Ring.Spec.Shards {
		g, e := guard(ctx, s.Pools[id])
		if e != nil {
			return e
		}
		if g.Pending != "" {
			return fmt.Errorf("acknowledge completed rebalance before garbage collection")
		}
	}
	for _, id := range s.Ring.Spec.Shards {
		for b := 0; b < s.Ring.Spec.Buckets; b++ {
			if s.Ring.Owner(b) == id {
				continue
			}
			tx, e := s.Pools[id].Begin(ctx)
			if e != nil {
				return e
			}
			for _, table := range tables {
				if _, e := tx.Exec(ctx, `DELETE FROM public.`+table.name+` WHERE bucket_id=$1`, b); e != nil {
					tx.Rollback(ctx)
					return e
				}
			}
			if e := tx.Commit(ctx); e != nil {
				return e
			}
		}
	}
	return nil
}
