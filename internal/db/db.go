package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Subscription struct {
	ID               int
	DealType         string
	Region           int
	MetroStations    []string
	MetroFilterLabel string // e.g. "Округа: Восточный, Южный"; empty if no metro filter
	MinPrice         int
	MaxPrice         int
	MinArea          float64
	MaxArea          float64
	Rooms            []int64
	MinScore         int

	// Extended filters (zero-valued when not set, meaning "no filter").
	MinUndergroundPlace int
	MinKitchenArea      float64
	MinFloor            int
	MaxFloor            int
	MinCeilingHeight    float64
	ChildrenRequired    bool
	PetsRequired        bool
	DishwasherRequired  bool
	ConditionerRequired bool
	MinRenovation       string
	BalconyRequired     bool
	BathroomType        string

	// ScoringParams is nil for default scoring, or the 18 custom weights
	// stored in subscription_scoring_params otherwise.
	ScoringParams *ScoringParams

	// PriorityStations holds the canonical station names named when the
	// subscriber chose the "priority stations" scoring option (report
	// subscriptions never set this — always empty for them).
	//
	// PriorityStationNames/PriorityStationScores hold the full priority-boosted
	// station ranking computed once at creation time (best-first, i.e. index 0
	// is place 1; scores already normalized by dividing by the station count),
	// so flats-analyzer can look a flat's station place up directly instead of
	// recomputing the ranking algorithm for every flat.
	PriorityStations      []string
	PriorityStationNames  []string
	PriorityStationScores []float64
}

// ScoringParams holds the 18 customizable scoring multipliers a subscriber
// can override, stored one-to-one with a subscription in
// subscription_scoring_params.
type ScoringParams struct {
	AllArea            float64
	KitchenArea        float64
	Pets               float64
	Dishwasher         float64
	Conditioner        float64
	Apartments         float64
	TwoRoom            float64
	ThreeRoom          float64
	FourRoom           float64
	AdditionalRooms    float64
	WindowsYard        float64
	WindowsStreet      float64
	WindowsBoth        float64
	RenovationDesign   float64
	RenovationEuro     float64
	RenovationCosmetic float64
	BathroomSeparated  float64
	Balcony            float64
	Loggia             float64
	Underground        float64
}

// ReportSubscription mirrors a report_user_subscriptions row: the same
// filters as Subscription (minus scoring params, which reports don't
// support), plus a send period and the last-sent timestamp.
type ReportSubscription struct {
	ID               int
	DealType         string
	Region           int
	MetroStations    []string
	MetroFilterLabel string
	MinPrice         int
	MaxPrice         int
	MinArea          float64
	MaxArea          float64
	Rooms            []int64
	MinScore         int

	MinUndergroundPlace int
	MinKitchenArea      float64
	MinFloor            int
	MaxFloor            int
	MinCeilingHeight    float64
	ChildrenRequired    bool
	PetsRequired        bool
	DishwasherRequired  bool
	ConditionerRequired bool
	MinRenovation       string
	BalconyRequired     bool
	BathroomType        string

	PeriodSeconds int
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

// CreateSubscription inserts a new active subscription for chatID, along with
// its custom scoring params row if sub.ScoringParams is set. Multiple active
// subscriptions per chatID are allowed.
func (db *DB) CreateSubscription(ctx context.Context, chatID int64, sub Subscription) error {
	rooms := sub.Rooms
	if rooms == nil {
		rooms = []int64{}
	}
	metroStations := sub.MetroStations
	if metroStations == nil {
		metroStations = []string{}
	}
	priorityStations := sub.PriorityStations
	if priorityStations == nil {
		priorityStations = []string{}
	}
	priorityStationNames := sub.PriorityStationNames
	if priorityStationNames == nil {
		priorityStationNames = []string{}
	}
	priorityStationScores := sub.PriorityStationScores
	if priorityStationScores == nil {
		priorityStationScores = []float64{}
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var subscriptionID int
	err = tx.QueryRow(ctx,
		`INSERT INTO user_subscriptions
		   (chat_id, deal_type, region, min_price, max_price, min_area, max_area, rooms, min_score,
		    min_underground_place, min_kitchen_area, min_floor, max_floor, min_ceiling_height,
		    children_required, pets_required, dishwasher_required, conditioner_required,
		    min_renovation, balcony_required, bathroom_type, metro_stations, metro_filter_label,
		    priority_stations, priority_station_names, priority_station_scores, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		         $10, $11, $12, $13, $14,
		         $15, $16, $17, $18,
		         $19, $20, $21, $22, $23, $24, $25, $26, TRUE)
		 RETURNING id`,
		chatID, sub.DealType, sub.Region, sub.MinPrice, sub.MaxPrice,
		sub.MinArea, sub.MaxArea, rooms, sub.MinScore,
		sub.MinUndergroundPlace, sub.MinKitchenArea, sub.MinFloor, sub.MaxFloor, sub.MinCeilingHeight,
		sub.ChildrenRequired, sub.PetsRequired, sub.DishwasherRequired, sub.ConditionerRequired,
		sub.MinRenovation, sub.BalconyRequired, sub.BathroomType, metroStations, sub.MetroFilterLabel,
		priorityStations, priorityStationNames, priorityStationScores,
	).Scan(&subscriptionID)
	if err != nil {
		return fmt.Errorf("inserting subscription: %w", err)
	}

	if p := sub.ScoringParams; p != nil {
		_, err = tx.Exec(ctx,
			`INSERT INTO subscription_scoring_params
			   (subscription_id, all_area_multiplier, kitchen_area_multiplier, pets_multiplier,
			    dishwasher_multiplier, conditioner_multiplier,
			    apartments_multiplier, two_room_multiplier, three_room_multiplier, four_room_multiplier,
			    additional_rooms_multiplier, windows_yard_multiplier, windows_street_multiplier,
			    windows_both_multiplier, renovation_design_mult, renovation_euro_mult,
			    renovation_cosmetic_mult, bathroom_separated_mult, balcony_multiplier,
			    loggia_multiplier, underground_score_mult)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
			subscriptionID, p.AllArea, p.KitchenArea, p.Pets,
			p.Dishwasher, p.Conditioner,
			p.Apartments, p.TwoRoom, p.ThreeRoom, p.FourRoom,
			p.AdditionalRooms, p.WindowsYard, p.WindowsStreet,
			p.WindowsBoth, p.RenovationDesign, p.RenovationEuro,
			p.RenovationCosmetic, p.BathroomSeparated, p.Balcony,
			p.Loggia, p.Underground)
		if err != nil {
			return fmt.Errorf("inserting scoring params: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// GetActiveSubscriptions returns all active subscriptions for chatID, ordered by id.
func (db *DB) GetActiveSubscriptions(ctx context.Context, chatID int64) ([]Subscription, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, deal_type, region, min_price, max_price, min_area, max_area, rooms, min_score,
		        min_underground_place, min_kitchen_area, min_floor, max_floor, min_ceiling_height,
		        children_required, pets_required, dishwasher_required, conditioner_required,
		        min_renovation, balcony_required, bathroom_type, metro_stations, metro_filter_label,
		        priority_stations
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
		if err := rows.Scan(&s.ID, &s.DealType, &s.Region, &s.MinPrice, &s.MaxPrice,
			&s.MinArea, &s.MaxArea, &s.Rooms, &s.MinScore,
			&s.MinUndergroundPlace, &s.MinKitchenArea, &s.MinFloor, &s.MaxFloor, &s.MinCeilingHeight,
			&s.ChildrenRequired, &s.PetsRequired, &s.DishwasherRequired, &s.ConditionerRequired,
			&s.MinRenovation, &s.BalconyRequired, &s.BathroomType, &s.MetroStations, &s.MetroFilterLabel,
			&s.PriorityStations); err != nil {
			return nil, fmt.Errorf("scanning subscription: %w", err)
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// CreateReportSubscription inserts a new active report subscription for
// chatID, reusing the same filter fields as Subscription. Report
// subscriptions don't support custom scoring params.
func (db *DB) CreateReportSubscription(ctx context.Context, chatID int64, sub Subscription, periodSeconds int) error {
	rooms := sub.Rooms
	if rooms == nil {
		rooms = []int64{}
	}
	metroStations := sub.MetroStations
	if metroStations == nil {
		metroStations = []string{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO report_user_subscriptions
		   (chat_id, deal_type, region, min_price, max_price, min_area, max_area, rooms, min_score,
		    min_underground_place, min_kitchen_area, min_floor, max_floor, min_ceiling_height,
		    children_required, pets_required, dishwasher_required, conditioner_required,
		    min_renovation, balcony_required, bathroom_type, metro_stations, metro_filter_label,
		    period_seconds, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		         $10, $11, $12, $13, $14,
		         $15, $16, $17, $18,
		         $19, $20, $21, $22, $23, $24, TRUE)`,
		chatID, sub.DealType, sub.Region, sub.MinPrice, sub.MaxPrice,
		sub.MinArea, sub.MaxArea, rooms, sub.MinScore,
		sub.MinUndergroundPlace, sub.MinKitchenArea, sub.MinFloor, sub.MaxFloor, sub.MinCeilingHeight,
		sub.ChildrenRequired, sub.PetsRequired, sub.DishwasherRequired, sub.ConditionerRequired,
		sub.MinRenovation, sub.BalconyRequired, sub.BathroomType, metroStations, sub.MetroFilterLabel, periodSeconds,
	)
	if err != nil {
		return fmt.Errorf("inserting report subscription: %w", err)
	}
	return nil
}

// GetActiveReportSubscriptions returns all active report subscriptions for
// chatID, ordered by id.
func (db *DB) GetActiveReportSubscriptions(ctx context.Context, chatID int64) ([]ReportSubscription, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, deal_type, region, min_price, max_price, min_area, max_area, rooms, min_score,
		        min_underground_place, min_kitchen_area, min_floor, max_floor, min_ceiling_height,
		        children_required, pets_required, dishwasher_required, conditioner_required,
		        min_renovation, balcony_required, bathroom_type, metro_stations, metro_filter_label, period_seconds
		 FROM report_user_subscriptions
		 WHERE chat_id = $1 AND is_active = TRUE
		 ORDER BY id`,
		chatID)
	if err != nil {
		return nil, fmt.Errorf("querying report subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []ReportSubscription
	for rows.Next() {
		var s ReportSubscription
		if err := rows.Scan(&s.ID, &s.DealType, &s.Region, &s.MinPrice, &s.MaxPrice,
			&s.MinArea, &s.MaxArea, &s.Rooms, &s.MinScore,
			&s.MinUndergroundPlace, &s.MinKitchenArea, &s.MinFloor, &s.MaxFloor, &s.MinCeilingHeight,
			&s.ChildrenRequired, &s.PetsRequired, &s.DishwasherRequired, &s.ConditionerRequired,
			&s.MinRenovation, &s.BalconyRequired, &s.BathroomType, &s.MetroStations, &s.MetroFilterLabel,
			&s.PeriodSeconds); err != nil {
			return nil, fmt.Errorf("scanning report subscription: %w", err)
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// DeactivateReportSubscriptionByID sets is_active = FALSE for the given
// report subscription, scoped to chatID.
func (db *DB) DeactivateReportSubscriptionByID(ctx context.Context, chatID int64, subscriptionID int) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE report_user_subscriptions SET is_active = FALSE
		 WHERE id = $1 AND chat_id = $2 AND is_active = TRUE`,
		subscriptionID, chatID)
	if err != nil {
		return false, fmt.Errorf("deactivating report subscription: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
