package session

// DefaultRentScoringParams mirrors realty-parser's rentConstants for the
// customizable scoring multipliers.
var DefaultRentScoringParams = ScoringParams{
	AllArea:            300,
	KitchenArea:        50,
	Pets:               4000,
	Dishwasher:         1500,
	Conditioner:        1500,
	Apartments:         3000,
	TwoRoom:            3000,
	ThreeRoom:          5000,
	FourRoom:           7500,
	AdditionalRooms:    1500,
	WindowsYard:        400,
	WindowsStreet:      200,
	WindowsBoth:        500,
	RenovationDesign:   7000,
	RenovationEuro:     5500,
	RenovationCosmetic: 3000,
	BathroomSeparated:  1000,
	Balcony:            1500,
	Loggia:             2500,
	Underground:        1000,
}

// saleScaleMultiplier mirrors realty-parser's scoring.saleScaleMultiplier.
const saleScaleMultiplier = 200

// DefaultSaleScoringParams mirrors realty-parser's saleConstants: every
// customizable multiplier scaled by saleScaleMultiplier relative to rent.
var DefaultSaleScoringParams = ScoringParams{
	AllArea:            DefaultRentScoringParams.AllArea * saleScaleMultiplier,
	KitchenArea:        DefaultRentScoringParams.KitchenArea * saleScaleMultiplier,
	Pets:               DefaultRentScoringParams.Pets * saleScaleMultiplier,
	Dishwasher:         DefaultRentScoringParams.Dishwasher * saleScaleMultiplier,
	Conditioner:        DefaultRentScoringParams.Conditioner * saleScaleMultiplier,
	Apartments:         DefaultRentScoringParams.Apartments * saleScaleMultiplier,
	TwoRoom:            DefaultRentScoringParams.TwoRoom * saleScaleMultiplier,
	ThreeRoom:          DefaultRentScoringParams.ThreeRoom * saleScaleMultiplier,
	FourRoom:           DefaultRentScoringParams.FourRoom * saleScaleMultiplier,
	AdditionalRooms:    DefaultRentScoringParams.AdditionalRooms * saleScaleMultiplier,
	WindowsYard:        DefaultRentScoringParams.WindowsYard * saleScaleMultiplier,
	WindowsStreet:      DefaultRentScoringParams.WindowsStreet * saleScaleMultiplier,
	WindowsBoth:        DefaultRentScoringParams.WindowsBoth * saleScaleMultiplier,
	RenovationDesign:   DefaultRentScoringParams.RenovationDesign * saleScaleMultiplier,
	RenovationEuro:     DefaultRentScoringParams.RenovationEuro * saleScaleMultiplier,
	RenovationCosmetic: DefaultRentScoringParams.RenovationCosmetic * saleScaleMultiplier,
	BathroomSeparated:  DefaultRentScoringParams.BathroomSeparated * saleScaleMultiplier,
	Balcony:            DefaultRentScoringParams.Balcony * saleScaleMultiplier,
	Loggia:             DefaultRentScoringParams.Loggia * saleScaleMultiplier,
	Underground:        DefaultRentScoringParams.Underground * saleScaleMultiplier,
}

// DefaultScoringParams returns the base scoring defaults for the given deal type.
func DefaultScoringParams(dealType string) ScoringParams {
	if dealType == DealTypeSale {
		return DefaultSaleScoringParams
	}
	return DefaultRentScoringParams
}

// ScoringQuestion describes one custom-scoring question. Object is just the
// "за ...?" tail — the caller prefixes it with "Сколько вы готовы платить в
// месяц" (rent) or "Сколько вы готовы заплатить" (sale) depending on deal type.
type ScoringQuestion struct {
	Step   Step
	Object string
	Get    func(p ScoringParams) float64
	Set    func(p *ScoringParams, v float64)
}

// ScoringQuestions lists the questions in the order given in the product
// spec, each carrying its own default-value getter/setter pair.
var ScoringQuestions = []ScoringQuestion{
	{StepScoreAllArea, "каждый дополнительный квадратный метр площади квартиры?",
		func(p ScoringParams) float64 { return p.AllArea }, func(p *ScoringParams, v float64) { p.AllArea = v }},
	{StepScoreKitchenArea, "каждый дополнительный квадратный метр площади кухни?",
		func(p ScoringParams) float64 { return p.KitchenArea }, func(p *ScoringParams, v float64) { p.KitchenArea = v }},
	{StepScorePets, "возможность заселения с животными?",
		func(p ScoringParams) float64 { return p.Pets }, func(p *ScoringParams, v float64) { p.Pets = v }},
	{StepScoreDishwasher, "наличие посудомоечной машины?",
		func(p ScoringParams) float64 { return p.Dishwasher }, func(p *ScoringParams, v float64) { p.Dishwasher = v }},
	{StepScoreConditioner, "наличие кондиционера?",
		func(p ScoringParams) float64 { return p.Conditioner }, func(p *ScoringParams, v float64) { p.Conditioner = v }},
	{StepScoreApartments, "студию?",
		func(p ScoringParams) float64 { return p.Apartments }, func(p *ScoringParams, v float64) { p.Apartments = v }},
	{StepScoreTwoRoom, "двухкомнатную квартиру?",
		func(p ScoringParams) float64 { return p.TwoRoom }, func(p *ScoringParams, v float64) { p.TwoRoom = v }},
	{StepScoreThreeRoom, "трёхкомнатную квартиру?",
		func(p ScoringParams) float64 { return p.ThreeRoom }, func(p *ScoringParams, v float64) { p.ThreeRoom = v }},
	{StepScoreFourRoom, "четырёхкомнатную квартиру?",
		func(p ScoringParams) float64 { return p.FourRoom }, func(p *ScoringParams, v float64) { p.FourRoom = v }},
	{StepScoreAdditionalRooms, "каждую комнату сверх четвёртой?",
		func(p ScoringParams) float64 { return p.AdditionalRooms }, func(p *ScoringParams, v float64) { p.AdditionalRooms = v }},
	{StepScoreWindowsYard, "окна во двор?",
		func(p ScoringParams) float64 { return p.WindowsYard }, func(p *ScoringParams, v float64) { p.WindowsYard = v }},
	{StepScoreWindowsStreet, "окна на улицу?",
		func(p ScoringParams) float64 { return p.WindowsStreet }, func(p *ScoringParams, v float64) { p.WindowsStreet = v }},
	{StepScoreWindowsBoth, "окна во двор и на улицу?",
		func(p ScoringParams) float64 { return p.WindowsBoth }, func(p *ScoringParams, v float64) { p.WindowsBoth = v }},
	{StepScoreRenovationDesign, "дизайнерский ремонт?",
		func(p ScoringParams) float64 { return p.RenovationDesign }, func(p *ScoringParams, v float64) { p.RenovationDesign = v }},
	{StepScoreRenovationEuro, "евроремонт?",
		func(p ScoringParams) float64 { return p.RenovationEuro }, func(p *ScoringParams, v float64) { p.RenovationEuro = v }},
	{StepScoreRenovationCosmetic, "косметический ремонт?",
		func(p ScoringParams) float64 { return p.RenovationCosmetic }, func(p *ScoringParams, v float64) { p.RenovationCosmetic = v }},
	{StepScoreBathroomSeparated, "раздельный санузел?",
		func(p ScoringParams) float64 { return p.BathroomSeparated }, func(p *ScoringParams, v float64) { p.BathroomSeparated = v }},
	{StepScoreBalcony, "наличие балкона?",
		func(p ScoringParams) float64 { return p.Balcony }, func(p *ScoringParams, v float64) { p.Balcony = v }},
	{StepScoreLoggia, "наличие лоджии?",
		func(p ScoringParams) float64 { return p.Loggia }, func(p *ScoringParams, v float64) { p.Loggia = v }},
	{StepScoreUnderground, "каждую минуту близости к центру?",
		func(p ScoringParams) float64 { return p.Underground }, func(p *ScoringParams, v float64) { p.Underground = v }},
}

// ScoringQuestionByStep looks up the question metadata for a given step.
func ScoringQuestionByStep(step Step) (ScoringQuestion, bool) {
	for _, q := range ScoringQuestions {
		if q.Step == step {
			return q, true
		}
	}
	return ScoringQuestion{}, false
}

// ScoringStepIndex returns the 0-based position of step within ScoringSteps.
func ScoringStepIndex(step Step) (int, bool) {
	for i, s := range ScoringSteps {
		if s == step {
			return i, true
		}
	}
	return 0, false
}
