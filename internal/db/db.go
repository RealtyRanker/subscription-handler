package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Subscription struct {
	ID       int
	Region   int
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

// DeactivateSubscriptionByID sets is_active = FALSE for the given subscription,
// scoped to chatID so a chat can only cancel its own subscriptions. Returns
// whether a row was actually deactivated.
func (db *DB) DeactivateSubscriptionByID(ctx context.Context, chatID int64, subscriptionID int) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE user_subscriptions SET is_active = FALSE
		 WHERE id = $1 AND chat_id = $2 AND is_active = TRUE`,
		subscriptionID, chatID)
	if err != nil {
		return false, fmt.Errorf("deactivating subscription: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CreateSubscription inserts a new active subscription for chatID. Multiple
// active subscriptions per chatID are allowed.
func (db *DB) CreateSubscription(ctx context.Context, chatID int64, sub Subscription) error {
	rooms := sub.Rooms
	if rooms == nil {
		rooms = []int32{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO user_subscriptions
		   (chat_id, region, min_price, max_price, min_area, max_area, rooms, min_score, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE)`,
		chatID, sub.Region, sub.MinPrice, sub.MaxPrice,
		sub.MinArea, sub.MaxArea, rooms, sub.MinScore)
	if err != nil {
		return fmt.Errorf("inserting subscription: %w", err)
	}
	return nil
}

// GetActiveSubscriptions returns all active subscriptions for chatID, ordered by id.
func (db *DB) GetActiveSubscriptions(ctx context.Context, chatID int64) ([]Subscription, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, region, min_price, max_price, min_area, max_area, rooms, min_score
		 FROM user_subscriptions
		 WHERE chat_id = $1 AND is_active = TRUE
		 ORDER BY id`,
		chatID)
	if err != nil {
		return nil, fmt.Errorf("querying subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.Region, &s.MinPrice, &s.MaxPrice,
			&s.MinArea, &s.MaxArea, &s.Rooms, &s.MinScore); err != nil {
			return nil, fmt.Errorf("scanning subscription: %w", err)
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}