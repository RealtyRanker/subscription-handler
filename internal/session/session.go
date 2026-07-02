package session

import (
	"fmt"
	"sync"
)

type Step int

const (
	StepIdle Step = iota
	StepDealType
	StepReportPeriod
	StepRegion
	StepLocationFilterType
	StepOkrugSelect
	StepLineSelect
	StepStationSelect
	StepDistrictSelect
	StepFilterMode
	StepMinPrice
	StepMaxPrice
	StepMinArea
	StepMaxArea
	StepRooms
	StepScoringMode
	StepPriorityStations
	StepScoreAllArea
	StepScoreKitchenArea
	StepScorePets
	StepScoreDishwasher
	StepScoreConditioner
	StepScoreApartments
	StepScoreTwoRoom
	StepScoreThreeRoom
	StepScoreFourRoom
	StepScoreAdditionalRooms
	StepScoreWindowsYard
	StepScoreWindowsStreet
	StepScoreWindowsBoth
	StepScoreRenovationDesign
	StepScoreRenovationEuro
	StepScoreRenovationCosmetic
	StepScoreBathroomSeparated
	StepScoreBalcony
	StepScoreLoggia
	StepScoreUnderground
	StepMinScore
	StepMinUndergroundPlace
	StepMinKitchenArea
	StepMinFloor
	StepMaxFloor
	StepMinCeilingHeight
	StepChildrenRequired
	StepPetsRequired
	StepDishwasherRequired
	StepConditionerRequired
	StepMinRenovation
	StepBalconyRequired
	StepBathroomType
	StepDone
)

// Deal types stored in user_subscriptions.deal_type.
const (
	DealTypeRent = "rent"
	DealTypeSale = "sale"
)

// Wizard kind: a plain instant-notification subscription vs a periodic report.
const (
	KindSubscription = "subscription"
	KindReport       = "report"
)

// ReportPeriod describes one of the selectable report send frequencies.
type ReportPeriod struct {
	Seconds int
	Label   string
}

// ReportPeriods lists the offered report frequencies in ascending order.
var ReportPeriods = []ReportPeriod{
	{300, "5 минут"},
	{3600, "1 час"},
	{43200, "12 часов"},
	{86400, "24 часа"},
	{604800, "7 дней"},
	{2592000, "30 дней"},
}

// ReportPeriodLabel returns the label for a period in seconds, or the raw
// number if it doesn't match one of ReportPeriods.
func ReportPeriodLabel(seconds int) string {
	for _, p := range ReportPeriods {
		if p.Seconds == seconds {
			return p.Label
		}
	}
	return fmt.Sprintf("%d сек.", seconds)
}

// Filter modes offered right after region selection.
const (
	FilterModeBasic    = "basic"
	FilterModeExtended = "extended"
)

// Location filter types offered right after region selection: which kind of
// metro-based breakdown (if any) the user wants to narrow their search by.
// Only LocationFilterOkrugs is currently wired up to an actual station set;
// the others behave like LocationFilterAny until implemented.
const (
	LocationFilterOkrugs   = "okrugs"
	LocationFilterDistrict = "district"
	LocationFilterStation  = "station"
	LocationFilterLine     = "line"
	LocationFilterAny      = "any"
)

// Renovation levels stored in user_subscriptions.min_renovation, ranked
// design > euro > cosmetic > "" (не важно / no renovation info).
const (
	RenovationDesign   = "design"
	RenovationEuro     = "euro"
	RenovationCosmetic = "cosmetic"
	RenovationAny      = ""
)

// Bathroom types stored in user_subscriptions.bathroom_type.
const (
	BathroomSeparated = "separated"
	BathroomCombined  = "combined"
	BathroomAny       = ""
)

// Scoring modes offered right before the min-score question. ScoringModePriority
// is only offered for Moscow (region 1) subscriptions; it behaves like the
// default formula for matching purposes, but lets the user name priority
// stations whose neighborhood gets boosted in a one-off top-10 preview.
const (
	ScoringModeDefault  = "default"
	ScoringModeCustom   = "custom"
	ScoringModePriority = "priority"
)

// ScoringSteps lists the custom-scoring questions in order, used both for
// step transitions and for their own separate "Скоринг i/N" numbering.
// StepMinScore is appended last: it's only asked when the user opts into
// custom scoring, and skipped entirely for the default formula.
var ScoringSteps = []Step{
	StepScoreAllArea,
	StepScoreKitchenArea,
	StepScorePets,
	StepScoreDishwasher,
	StepScoreConditioner,
	StepScoreApartments,
	StepScoreTwoRoom,
	StepScoreThreeRoom,
	StepScoreFourRoom,
	StepScoreAdditionalRooms,
	StepScoreWindowsYard,
	StepScoreWindowsStreet,
	StepScoreWindowsBoth,
	StepScoreRenovationDesign,
	StepScoreRenovationEuro,
	StepScoreRenovationCosmetic,
	StepScoreBathroomSeparated,
	StepScoreBalcony,
	StepScoreLoggia,
	StepScoreUnderground,
	StepMinScore,
}

// ScoringParams holds the 18 customizable scoring multipliers a user can
// override; see scoring_defaults.go for the rent/sale default values shown
// to the user and substituted when they answer 0 ("use default").
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

type Session struct {
	Step Step
	Kind string // KindSubscription or KindReport

	DealType            string
	ReportPeriodSeconds int
	Region              int
	LocationFilterType  string
	SelectedOkrugs      []string // okrug names currently toggled on in StepOkrugSelect
	SelectedLines       []string // line numbers currently toggled on in StepLineSelect
	SelectedStations    []string // station names currently toggled on in StepStationSelect
	SelectedDistricts   []string // district names currently toggled on in StepDistrictSelect
	MetroStations       []string // union of stations for the chosen location filter
	FilterMode          string

	MinPrice int
	MaxPrice int
	MinArea  float64
	MaxArea  float64
	Rooms    []int64

	ScoringMode      string
	ScoringParams    ScoringParams
	PriorityStations []string // canonical station names, set when ScoringMode == ScoringModePriority

	MinScore int

	// Extended filters (only asked when FilterMode == FilterModeExtended).
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
}

type Store struct {
	mu       sync.Mutex
	sessions map[int64]*Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[int64]*Session)}
}

func (s *Store) Get(chatID int64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[chatID]
}

func (s *Store) Set(chatID int64, sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[chatID] = sess
}

func (s *Store) Delete(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, chatID)
}
