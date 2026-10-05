package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"example.com/subscriptions/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Create(ctx context.Context, user uuid.UUID, key string, req domain.Create) (domain.Subscription, bool, error) {
	var empty domain.Subscription
	if err := req.Validate(); err != nil {
		return empty, false, err
	}
	if len(key) < 16 || len(key) > 128 {
		return empty, false, domain.Invalid("Idempotency-Key must contain 16..128 characters")
	}
	body, _ := json.Marshal(req)
	requestHash := sha256.Sum256(body)
	keyHash := sha256.Sum256([]byte(key))
	tx, b, err := s.begin(ctx, user)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO public.idempotency(bucket_id,user_id,key_hash,request_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, b, user, keyHash[:], requestHash[:])
	if err != nil {
		return empty, false, err
	}
	if tag.RowsAffected() == 0 {
		var storedHash, response []byte
		if err := tx.QueryRow(ctx, `SELECT request_hash,response FROM public.idempotency WHERE bucket_id=$1 AND user_id=$2 AND key_hash=$3 FOR UPDATE`, b, user, keyHash[:]).Scan(&storedHash, &response); err != nil {
			return empty, false, err
		}
		if !bytes.Equal(storedHash, requestHash[:]) {
			return empty, false, domain.ErrConflict
		}
		var sub domain.Subscription
		if err := json.Unmarshal(response, &sub); err != nil {
			return empty, false, fmt.Errorf("invalid stored idempotency response")
		}
		if err := tx.Commit(ctx); err != nil {
			return empty, false, err
		}
		return sub, true, nil
	}
	// Per-user counter serializes creates/deletes and bounds expensive per-user reads.
	if _, err := tx.Exec(ctx, `INSERT INTO public.user_usage(bucket_id,user_id,subscription_count) VALUES($1,$2,0) ON CONFLICT DO NOTHING`, b, user); err != nil {
		return empty, false, err
	}
	tag, err = tx.Exec(ctx, `UPDATE public.user_usage SET subscription_count=subscription_count+1 WHERE bucket_id=$1 AND user_id=$2 AND subscription_count<10000`, b, user)
	if err != nil {
		return empty, false, err
	}
	if tag.RowsAffected() != 1 {
		return empty, false, domain.ErrQuota
	}
	row := tx.QueryRow(ctx, `INSERT INTO public.subscriptions(bucket_id,user_id,id,service_name,price,start_date,end_date) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+columns, b, user, uuid.New(), req.ServiceName, req.Price, req.StartDate.Time(), endValue(req.EndDate))
	sub, err := scan(row)
	if err != nil {
		return empty, false, err
	}
	response, err := json.Marshal(sub)
	if err != nil {
		return empty, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.idempotency SET response=$4 WHERE bucket_id=$1 AND user_id=$2 AND key_hash=$3`, b, user, keyHash[:], response); err != nil {
		return empty, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, false, err
	}
	return sub, false, nil
}

func (s *Store) Get(ctx context.Context, user, id uuid.UUID) (domain.Subscription, error) {
	tx, b, err := s.begin(ctx, user)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer tx.Rollback(ctx)
	sub, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2 AND id=$3`, b, user, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return sub, domain.ErrNotFound
	}
	if err != nil {
		return sub, err
	}
	return sub, tx.Commit(ctx)
}

func (s *Store) List(ctx context.Context, user uuid.UUID, req domain.List) ([]domain.Subscription, error) {
	if req.Limit < 1 || req.Limit > 100 {
		return nil, domain.Invalid("limit must be 1..100")
	}
	if req.Name != "" && !domain.ValidName(req.Name) {
		return nil, domain.Invalid("invalid service_name")
	}
	tx, b, err := s.begin(ctx, user)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	query := `SELECT ` + columns + ` FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2`
	args := []any{b, user}
	if req.Name != "" {
		args = append(args, req.Name)
		query += fmt.Sprintf(" AND service_name=$%d", len(args))
	}
	if !req.AfterTime.IsZero() {
		args = append(args, req.AfterTime, req.AfterID)
		query += fmt.Sprintf(" AND (created_at,id)<($%d,$%d)", len(args)-1, len(args))
	}
	args = append(args, req.Limit+1)
	query += fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", len(args))
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Subscription, 0, req.Limit+1)
	for rows.Next() {
		sub, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Patch(ctx context.Context, user, id uuid.UUID, version int64, req domain.Patch) (domain.Subscription, error) {
	tx, b, err := s.begin(ctx, user)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer tx.Rollback(ctx)
	sub, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2 AND id=$3 FOR UPDATE`, b, user, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return sub, domain.ErrNotFound
	}
	if err != nil {
		return sub, err
	}
	if sub.Version != version {
		return sub, domain.ErrVersion
	}
	if err := req.Apply(&sub); err != nil {
		return sub, err
	}
	sub, err = scan(tx.QueryRow(ctx, `UPDATE public.subscriptions SET service_name=$4,price=$5,start_date=$6,end_date=$7,version=version+1,updated_at=clock_timestamp() WHERE bucket_id=$1 AND user_id=$2 AND id=$3 RETURNING `+columns, b, user, id, sub.ServiceName, sub.Price, sub.StartDate.Time(), endValue(sub.EndDate)))
	if err != nil {
		return sub, err
	}
	return sub, tx.Commit(ctx)
}

func (s *Store) Delete(ctx context.Context, user, id uuid.UUID, version int64) error {
	tx, b, err := s.begin(ctx, user)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var got int64
	err = tx.QueryRow(ctx, `SELECT version FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2 AND id=$3 FOR UPDATE`, b, user, id).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if got != version {
		return domain.ErrVersion
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2 AND id=$3`, b, user, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.user_usage SET subscription_count=subscription_count-1 WHERE bucket_id=$1 AND user_id=$2`, b, user); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Total(ctx context.Context, user uuid.UUID, period domain.Period) (int64, error) {
	if err := period.Validate(); err != nil {
		return 0, err
	}
	tx, b, err := s.begin(ctx, user)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	query := `WITH overlap AS (SELECT price,GREATEST(start_date,$3::date) AS lo,LEAST(COALESCE(end_date,$4::date),$4::date) AS hi FROM public.subscriptions WHERE bucket_id=$1 AND user_id=$2 AND start_date<=$4::date AND (end_date IS NULL OR end_date>=$3::date)`
	args := []any{b, user, period.From.Time(), period.To.Time()}
	if period.Name != "" {
		query += ` AND service_name=$5`
		args = append(args, period.Name)
	}
	query += `) SELECT COALESCE(SUM(price::bigint * ((EXTRACT(YEAR FROM hi)::bigint-EXTRACT(YEAR FROM lo)::bigint)*12+EXTRACT(MONTH FROM hi)::bigint-EXTRACT(MONTH FROM lo)::bigint+1)),0)::bigint FROM overlap`
	var total int64
	if err := tx.QueryRow(ctx, query, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, tx.Commit(ctx)
}
