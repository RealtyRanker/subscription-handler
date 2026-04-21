package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Subscription struct {
	MinPrice int
	MaxPrice int
	MinArea  float64
	MaxArea  float64
	Rooms    []int32
	MinScore int
}

type DB struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (db *DB) Close() {
	db.pool.Close()
}

// UpsertSubscription deactivates any existing active subscription for chatID,
// then inserts a new one.
func (db *DB) UpsertSubscription(ctx context.Context, chatID int64, sub Subscription) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx,
		`UPDATE user_subscriptions SET is_active = FALSE
		 WHERE chat_id = $1 AND is_active = TRUE`,
		chatID)
	if err != nil {
		return fmt.Errorf("deactivating old subscription: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO user_subscriptions
		   (chat_id, min_price, max_price, min_area, max_area, rooms, min_score, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE)`,
		chatID, sub.MinPrice, sub.MaxPrice,
		sub.MinArea, sub.MaxArea, sub.Rooms, sub.MinScore)
	if err != nil {
		return fmt.Errorf("inserting subscription: %w", err)
	}

	return tx.Commit(ctx)
}