package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/subscriptions/internal/config"
	"example.com/subscriptions/internal/domain"
	"example.com/subscriptions/internal/hashring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Ring  *hashring.Ring
	Pools map[string]*pgxpool.Pool
}

func Open(ctx context.Context, c config.Config, admin bool) (*Store, error) {
	r, e := c.Ring()
	if e != nil {
		return nil, e
	}
	s := &Store{Ring: r, Pools: make(map[string]*pgxpool.Pool)}
	for _, sh := range c.Shards {
		dsn, err := c.DSN(sh, admin)
		if err != nil {
			s.Close()
			return nil, err
		}
		pc, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("invalid DSN for shard %s", sh.ID)
		}
		pc.MaxConns = c.PoolMax
		pc.MinConns = 0
		pc.MaxConnLifetime = 30 * time.Minute
		pc.MaxConnLifetimeJitter = 5 * time.Minute
		pc.MaxConnIdleTime = 5 * time.Minute
		pc.ConnConfig.ConnectTimeout = 3 * time.Second
		pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
		pc.ConnConfig.RuntimeParams["application_name"] = "subscriptions"
		pc.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
		pc.ConnConfig.RuntimeParams["lock_timeout"] = "2000"
		pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
		if admin {
			pc.ConnConfig.RuntimeParams["statement_timeout"] = "600000"
			pc.ConnConfig.RuntimeParams["lock_timeout"] = "30000"
			pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "600000"
		} else {
			// Validate every new/reconnected session, including after database failover.
			pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
				var unsafe bool
				if err := conn.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe); err != nil {
					return err
				}
				if unsafe {
					return fmt.Errorf("runtime role must not be SUPERUSER or BYPASSRLS")
				}
				return nil
			}
		}
		pool, err := pgxpool.NewWithConfig(ctx, pc)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("open shard %s", sh.ID)
		}
		s.Pools[sh.ID] = pool
		if admin {
			if err := pool.Ping(ctx); err != nil {
				s.Close()
				return nil, fmt.Errorf("cannot connect to shard %s", sh.ID)
			}
		}
	}
	return s, nil
}
func (s *Store) Close() {
	for _, p := range s.Pools {
		p.Close()
	}
}

func (s *Store) Ready(ctx context.Context) error {
	for _, id := range s.Ring.Spec.Shards {
		var ok bool
		err := s.Pools[id].QueryRow(ctx, `SELECT shard_id=$1 AND fingerprint=$2 AND NOT frozen FROM public.routing_guard WHERE singleton`, id, s.Ring.Fingerprint).Scan(&ok)
		if err != nil || !ok {
			return domain.ErrUnavailable
		}
	}
	return nil
}

func (s *Store) Health(ctx context.Context) map[string]bool {
	result := make(map[string]bool, len(s.Pools))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, pool := range s.Pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var ok bool
			err := pool.QueryRow(probe, `SELECT shard_id=$1 AND fingerprint=$2 AND NOT frozen FROM public.routing_guard WHERE singleton`, id, s.Ring.Fingerprint).Scan(&ok)
			mu.Lock()
			result[id] = err == nil && ok
			mu.Unlock()
		}()
	}
	wg.Wait()
	return result
}

func (s *Store) begin(ctx context.Context, user uuid.UUID) (pgx.Tx, int, error) {
	if user == uuid.Nil {
		return nil, 0, domain.Invalid("user must not be nil")
	}
	bucket, id := s.Ring.Route(user)
	tx, err := s.Pools[id].BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, 0, fmt.Errorf("%w: begin", domain.ErrUnavailable)
	}
	var ok bool
	if err = tx.QueryRow(ctx, `SELECT public.check_routing($1,$2)`, id, s.Ring.Fingerprint).Scan(&ok); err != nil || !ok {
		tx.Rollback(ctx)
		return nil, 0, domain.ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.user_id',$1,TRUE)`, user.String()); err != nil {
		tx.Rollback(ctx)
		return nil, 0, err
	}
	return tx, bucket, nil
}

const columns = `id,user_id,service_name,price,start_date,end_date,version,created_at,updated_at`

func scan(row pgx.Row) (domain.Subscription, error) {
	var s domain.Subscription
	var start time.Time
	var end *time.Time
	err := row.Scan(&s.ID, &s.UserID, &s.ServiceName, &s.Price, &start, &end, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	s.StartDate = domain.FromTime(start)
	if end != nil {
		m := domain.FromTime(*end)
		s.EndDate = &m
	}
	return s, nil
}
func endValue(m *domain.Month) any {
	if m == nil {
		return nil
	}
	return m.Time()
}
