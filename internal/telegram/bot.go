package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/asmisnik/subscription-handler/internal/db"
	"github.com/asmisnik/subscription-handler/internal/metrics"
	"github.com/asmisnik/subscription-handler/internal/metro"
	"github.com/asmisnik/subscription-handler/internal/session"
)

const regionsPerPage = 10
const stationsPerPage = 50
const districtsPerPage = 30

// fixedSubscriptionSteps/fixedReportSteps list the main (non-extended,
// non-scoring) wizard steps in order for each wizard kind, used to compute
// "Шаг N/M" numbering. StepMinScore is not listed here: it's asked as the
// last custom-scoring question (see session.ScoringSteps) and skipped
// entirely when the default scoring formula is used.
var fixedSubscriptionSteps = []session.Step{
	session.StepDealType, session.StepRegion, session.StepLocationFilterType, session.StepFilterMode,
	session.StepMinPrice, session.StepMaxPrice, session.StepMinArea, session.StepMaxArea,
	session.StepRooms, session.StepScoringMode,
}

var fixedReportSteps = []session.Step{
	session.StepDealType, session.StepReportPeriod, session.StepRegion, session.StepLocationFilterType, session.StepFilterMode,
	session.StepMinPrice, session.StepMaxPrice, session.StepMinArea, session.StepMaxArea,
	session.StepRooms,
}

// moscowRegionID is the only region the metro-based location filter applies
// to; other regions skip StepLocationFilterType entirely.
const moscowRegionID = 1

// fixedSteps returns the fixed wizard steps for sess, dropping
// StepLocationFilterType outside Moscow (region unset counts as Moscow so
// the step still shows before the user has picked a region).
func fixedSteps(sess *session.Session) []session.Step {
	steps := fixedSubscriptionSteps
	if sess.Kind == session.KindReport {
		steps = fixedReportSteps
	}
	if sess.Region != 0 && sess.Region != moscowRegionID {
		filtered := make([]session.Step, 0, len(steps)-1)
		for _, s := range steps {
			if s == session.StepLocationFilterType {
				continue
			}
			filtered = append(filtered, s)
		}
		return filtered
	}
	return steps
}

// stepNumber returns the 1-based position of step in sess's fixed step list.
func stepNumber(step session.Step, sess *session.Session) int {
	for i, s := range fixedSteps(sess) {
		if s == step {
			return i + 1
		}
	}
	return 0
}

func totalSteps(sess *session.Session) int {
	return len(fixedSteps(sess))
}

// stepQuestionBody holds the question body (without "Шаг N/M —" numbering)
// for each fixed numeric step; numbering is prepended dynamically since it
// depends on the wizard kind (subscription vs report).
var stepQuestionBody = map[session.Step]string{
	session.StepMinPrice: "Минимальная цена (₽)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxPrice: "Максимальная цена (₽)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMinArea:  "Минимальная площадь (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxArea:  "Максимальная площадь (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepRooms:    "Количество комнат?\n\nВведите числа через пробел или запятую (например: 2 3 или 1,2,3).\nОтправьте 0, чтобы не фильтровать по комнатам.",
}

// extendedStepQuestions holds the (kind-independent, unnumbered) question
// text for extended filter steps.
var extendedStepQuestions = map[session.Step]string{
	session.StepMinUndergroundPlace: "Доп. фильтр — Минимальное место станции метро в топе?\n\nВведите число (квартира подойдёт, если станция метро не хуже этого места) или 0, чтобы не ограничивать.",
	session.StepMinKitchenArea:      "Доп. фильтр — Минимальная площадь кухни (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMinFloor:            "Доп. фильтр — Минимальный этаж?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxFloor:            "Доп. фильтр — Максимальный этаж?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMinCeilingHeight:    "Доп. фильтр — Минимальная высота потолков (м)?\n\nВведите число или 0, чтобы не ограничивать.",
}

// dealTypeLabels maps a stored deal_type to its human-readable Russian label.
var dealTypeLabels = map[string]string{
	session.DealTypeRent: "Аренда",
	session.DealTypeSale: "Продажа",
}

func dealTypeLabel(dealType string) string {
	if label, ok := dealTypeLabels[dealType]; ok {
		return label
	}
	return dealType
}

// renovationLabels maps a stored min_renovation value to its Russian label.
var renovationLabels = map[string]string{
	session.RenovationDesign:   "Дизайнерский",
	session.RenovationEuro:     "Евроремонт",
	session.RenovationCosmetic: "Косметический",
	session.RenovationAny:      "Не важно",
}

// bathroomLabels maps a stored bathroom_type value to its Russian label.
var bathroomLabels = map[string]string{
	session.BathroomSeparated: "Раздельный",
	session.BathroomCombined:  "Совмещённый",
	session.BathroomAny:       "Не важно",
}

// boolFilterMeta describes one "обязательно"/"не обязательно" extended filter.
type boolFilterMeta struct {
	step     session.Step
	key      string
	question string
	label    string // shown in the finished-subscription summary
}

var boolFilters = []boolFilterMeta{
	{session.StepChildrenRequired, "children", "Доп. фильтр — Можно с детьми?", "С детьми"},
	{session.StepPetsRequired, "pets", "Доп. фильтр — Можно с животными?", "С животными"},
	{session.StepDishwasherRequired, "dishwasher", "Доп. фильтр — Есть посудомоечная машина?", "Есть посудомойка"},
	{session.StepConditionerRequired, "conditioner", "Доп. фильтр — Есть кондиционер?", "Есть кондиционер"},
	{session.StepBalconyRequired, "balcony", "Доп. фильтр — Есть балкон или лоджия?", "Есть балкон/лоджия"},
}

func boolFilterByKey(key string) (boolFilterMeta, bool) {
	for _, f := range boolFilters {
		if f.key == key {
			return f, true
		}
	}
	return boolFilterMeta{}, false
}

func boolFilterByStep(step session.Step) (boolFilterMeta, bool) {
	for _, f := range boolFilters {
		if f.step == step {
			return f, true
		}
	}
	return boolFilterMeta{}, false
}

type Bot struct {
	client   *Client
	db       *db.DB
	sessions *session.Store
	logger   *zap.Logger
}

func NewBot(client *Client, database *db.DB, sessions *session.Store, logger *zap.Logger) *Bot {
	return &Bot{
		client:   client,
		db:       database,
		sessions: sessions,
		logger:   logger,
	}
}

func (b *Bot) Run(ctx context.Context) {
	b.logger.Info("telegram bot polling started")
	offset := 0
	for {
		updates, err := b.client.GetUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.logger.Error("getUpdates failed", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.Message != nil {
				b.handleMessage(ctx, u.Message)
			}
			if u.CallbackQuery != nil {
				b.handleCallbackQuery(ctx, u.CallbackQuery)
			}
			offset = u.UpdateID + 1
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *Message) {
	metrics.MessagesReceived.Inc()
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	b.logger.Debug("message received",
		zap.Int64("chat_id", chatID),
		zap.String("text", text),
	)

	if strings.HasPrefix(text, "/") {
		b.handleCommand(ctx, chatID, text)
		return
	}

	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step == session.StepIdle {
		b.send(ctx, chatID, "Отправьте /subscript, чтобы создать или обновить подписку.", nil)
		return
	}

	b.handleAnswer(ctx, chatID, sess, text)
}

func (b *Bot) handleCommand(ctx context.Context, chatID int64, text string) {
	cmd := strings.ToLower(strings.Fields(text)[0])
	metrics.CommandsReceived.WithLabelValues(cmd).Inc()

	switch cmd {
	case "/subscript", "/subscript@" + "":
		b.sessions.Set(chatID, &session.Session{Step: session.StepDealType, Kind: session.KindSubscription})
		b.sendDealTypeQuestion(ctx, chatID, session.KindSubscription)

	case "/cancel":
		b.sessions.Delete(chatID)
		subs, err := b.db.GetActiveSubscriptions(ctx, chatID)
		if err != nil {
			b.logger.Error("fetching active subscriptions failed", zap.Int64("chat_id", chatID), zap.Error(err))
			b.send(ctx, chatID, "Произошла ошибка. Попробуйте позже.", nil)
			return
		}
		if len(subs) == 0 {
			b.send(ctx, chatID, "Активных подписок не найдено.", nil)
			return
		}
		rows := make([][]InlineKeyboardButton, 0, len(subs))
		for _, s := range subs {
			rows = append(rows, []InlineKeyboardButton{
				{Text: subscriptionBrief(s), CallbackData: fmt.Sprintf("cancel:%d", s.ID)},
			})
		}
		b.send(ctx, chatID, "Выберите подписку, которую нужно отменить:", &InlineKeyboardMarkup{InlineKeyboard: rows})

	case "/reports", "/reports@" + "":
		b.sessions.Delete(chatID)
		markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "📊 Создать подписку на отчёты", CallbackData: "rpt_menu:create"}},
			{{Text: "🚫 Отменить подписку на отчёты", CallbackData: "rpt_menu:cancel"}},
		}}
		b.send(ctx, chatID, "Подписки на регулярные отчёты о квартирах. Что вы хотите сделать?", markup)

	default:
		b.send(ctx, chatID, "Неизвестная команда.\n\nДоступные команды:\n/subscript — создать новую подписку\n/cancel — отменить одну из активных подписок\n/reports — подписки на регулярные отчёты", nil)
	}
}

// nextStep computes which step follows the one just completed, branching on
// FilterMode (basic subscriptions skip straight to StepDone after MinScore)
// and DealType (children/pets filters only make sense for rent).
func nextStep(current session.Step, sess *session.Session) session.Step {
	switch current {
	case session.StepDealType:
		if sess.Kind == session.KindReport {
			return session.StepReportPeriod
		}
		return session.StepRegion
	case session.StepReportPeriod:
		return session.StepRegion
	case session.StepRegion:
		if sess.Region != moscowRegionID {
			return session.StepFilterMode
		}
		return session.StepLocationFilterType
	case session.StepLocationFilterType:
		return session.StepFilterMode
	case session.StepFilterMode:
		return session.StepMinPrice
	case session.StepMinPrice:
		return session.StepMaxPrice
	case session.StepMaxPrice:
		return session.StepMinArea
	case session.StepMinArea:
		return session.StepMaxArea
	case session.StepMaxArea:
		return session.StepRooms
	case session.StepRooms:
		if sess.Kind == session.KindReport {
			// Report subscriptions always use the default scoring formula,
			// so the min-score question (part of the custom-scoring flow) is
			// skipped entirely.
			return afterScoring(sess)
		}
		return session.StepScoringMode
	case session.StepScoringMode:
		switch sess.ScoringMode {
		case session.ScoringModeCustom:
			return session.ScoringSteps[0]
		case session.ScoringModePriority:
			return session.StepPriorityStations
		default:
			return afterScoring(sess)
		}
	case session.StepPriorityStations:
		return afterScoring(sess)
	case session.StepMinUndergroundPlace:
		return session.StepMinKitchenArea
	case session.StepMinKitchenArea:
		return session.StepMinFloor
	case session.StepMinFloor:
		return session.StepMaxFloor
	case session.StepMaxFloor:
		return session.StepMinCeilingHeight
	case session.StepMinCeilingHeight:
		if sess.DealType == session.DealTypeRent {
			return session.StepChildrenRequired
		}
		return session.StepDishwasherRequired
	case session.StepChildrenRequired:
		return session.StepPetsRequired
	case session.StepPetsRequired:
		return session.StepDishwasherRequired
	case session.StepDishwasherRequired:
		return session.StepConditionerRequired
	case session.StepConditionerRequired:
		return session.StepMinRenovation
	case session.StepMinRenovation:
		return session.StepBalconyRequired
	case session.StepBalconyRequired:
		return session.StepBathroomType
	case session.StepBathroomType:
		return session.StepDone
	default:
		if idx, ok := session.ScoringStepIndex(current); ok {
			if idx+1 < len(session.ScoringSteps) {
				return session.ScoringSteps[idx+1]
			}
			// Just answered StepMinScore, the last custom-scoring question.
			return afterScoring(sess)
		}
		return session.StepDone
	}
}

// afterScoring returns the step that follows the scoring stage (whether the
// user took the default formula or finished the custom-scoring questions),
// branching into the extended filters if requested.
func afterScoring(sess *session.Session) session.Step {
	if sess.FilterMode != session.FilterModeExtended {
		return session.StepDone
	}
	return session.StepMinUndergroundPlace
}

// advance moves the session to the given step and renders whatever prompt
// (text question, inline keyboard, or final summary) that step requires.
func (b *Bot) advance(ctx context.Context, chatID int64, sess *session.Session, step session.Step) {
	sess.Step = step
	b.sessions.Set(chatID, sess)

	switch step {
	case session.StepDone:
		b.finalize(ctx, chatID, sess)
	case session.StepReportPeriod:
		b.sendReportPeriodQuestion(ctx, chatID, sess)
	case session.StepRegion:
		b.sendRegionPage(ctx, chatID, sess, 0)
	case session.StepLocationFilterType:
		b.sendLocationFilterTypeQuestion(ctx, chatID, sess)
	case session.StepOkrugSelect:
		b.sendOkrugSelect(ctx, chatID, sess)
	case session.StepLineSelect:
		b.sendLineSelect(ctx, chatID, sess)
	case session.StepStationSelect:
		b.sendStationSelect(ctx, chatID, sess, 0)
	case session.StepDistrictSelect:
		b.sendDistrictSelect(ctx, chatID, sess, 0)
	case session.StepFilterMode:
		b.sendFilterModeQuestion(ctx, chatID, sess)
	case session.StepScoringMode:
		b.sendScoringModeQuestion(ctx, chatID, sess)
	case session.StepPriorityStations:
		b.sendPriorityStationsQuestion(ctx, chatID)
	case session.StepMinRenovation:
		b.sendRenovationQuestion(ctx, chatID)
	case session.StepBathroomType:
		b.sendBathroomQuestion(ctx, chatID)
	case session.StepMinScore:
		b.sendMinScoreQuestion(ctx, chatID)
	default:
		if meta, ok := boolFilterByStep(step); ok {
			b.sendBoolFilterQuestion(ctx, chatID, meta)
			return
		}
		if q, ok := session.ScoringQuestionByStep(step); ok {
			b.sendScoringQuestion(ctx, chatID, sess, q)
			return
		}
		b.sendQuestion(ctx, chatID, sess, step)
	}
}

func (b *Bot) handleAnswer(ctx context.Context, chatID int64, sess *session.Session, text string) {
	if q, ok := session.ScoringQuestionByStep(sess.Step); ok {
		v, err := parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		if v == 0 {
			v = q.Get(session.DefaultScoringParams(sess.DealType))
		}
		q.Set(&sess.ScoringParams, v)
		b.advance(ctx, chatID, sess, nextStep(sess.Step, sess))
		return
	}

	switch sess.Step {
	case session.StepDealType, session.StepReportPeriod, session.StepRegion, session.StepFilterMode,
		session.StepLocationFilterType, session.StepOkrugSelect, session.StepLineSelect, session.StepStationSelect,
		session.StepDistrictSelect, session.StepScoringMode,
		session.StepChildrenRequired, session.StepPetsRequired,
		session.StepDishwasherRequired, session.StepConditionerRequired,
		session.StepMinRenovation, session.StepBalconyRequired, session.StepBathroomType:
		b.send(ctx, chatID, "Пожалуйста, выберите вариант, нажав на кнопку в списке выше.", nil)
		return

	case session.StepMinPrice:
		v, err := parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.MinPrice = v

	case session.StepMaxPrice:
		v, err := parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.MaxPrice = v

	case session.StepMinArea:
		v, err := parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		sess.MinArea = v

	case session.StepMaxArea:
		v, err := parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		sess.MaxArea = v

	case session.StepRooms:
		v, err := parseRooms(text)
		if err != nil {
			b.send(ctx, chatID, "Введите числа через пробел или запятую, либо 0.", forceReply("Например: 2 3"))
			return
		}
		sess.Rooms = v

	case session.StepMinScore:
		v, err := parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.MinScore = v

	case session.StepMinUndergroundPlace:
		v, err := parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.MinUndergroundPlace = v

	case session.StepMinKitchenArea:
		v, err := parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		sess.MinKitchenArea = v

	case session.StepMinFloor:
		v, err := parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.MinFloor = v

	case session.StepMaxFloor:
		v, err := parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.MaxFloor = v

	case session.StepMinCeilingHeight:
		v, err := parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		sess.MinCeilingHeight = v

	case session.StepPriorityStations:
		tokens := parseStationNames(text)
		if len(tokens) == 0 {
			b.send(ctx, chatID, "Пожалуйста, укажите хотя бы одну станцию.", forceReply("Например: Щёлковская, Белорусская"))
			return
		}
		matched, unmatched := metro.MatchStationNames(tokens)
		if len(matched) == 0 {
			b.send(ctx, chatID, "Не удалось распознать ни одной станции. Проверьте названия и попробуйте снова.", forceReply("Например: Щёлковская, Белорусская"))
			return
		}
		sess.PriorityStations = matched
		if len(unmatched) > 0 {
			b.send(ctx, chatID, "⚠️ Не распознаны как станции метро: "+strings.Join(unmatched, ", "), nil)
		}

	default:
		return
	}

	b.advance(ctx, chatID, sess, nextStep(sess.Step, sess))
}

// handleCallbackQuery routes inline-keyboard button presses.
func (b *Bot) handleCallbackQuery(ctx context.Context, cq *CallbackQuery) {
	if cq.Message == nil {
		return
	}
	chatID := cq.Message.Chat.ID

	switch {
	case strings.HasPrefix(cq.Data, "deal:"):
		b.handleDealTypeCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "pg:"), strings.HasPrefix(cq.Data, "rgn:"):
		b.handleRegionCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "locf:"):
		b.handleLocationFilterTypeCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "okr:"):
		b.handleOkrugCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "line:"):
		b.handleLineCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "stn:"):
		b.handleStationCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "dst:"):
		b.handleDistrictCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "fm:"):
		b.handleFilterModeCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "scmode:"):
		b.handleScoringModeCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "bool:"):
		b.handleBoolFilterCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "renov:"):
		b.handleRenovationCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "bath:"):
		b.handleBathroomCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "cancel:"):
		b.handleCancelCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "rpt_menu:"):
		b.handleReportMenuCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "rptperiod:"):
		b.handleReportPeriodCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "cancel_rpt:"):
		b.handleCancelReportCallback(ctx, chatID, cq)
	}
}

// handleReportMenuCallback processes the /reports entry menu: "rpt_menu:create"
// starts the report-subscription wizard, "rpt_menu:cancel" lists active report
// subscriptions to cancel.
func (b *Bot) handleReportMenuCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	action := strings.TrimPrefix(cq.Data, "rpt_menu:")
	b.answerCallback(ctx, cq.ID, "")

	switch action {
	case "create":
		sess := &session.Session{Step: session.StepDealType, Kind: session.KindReport}
		b.sessions.Set(chatID, sess)
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "📊 Создание подписки на отчёты", nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.sendDealTypeQuestion(ctx, chatID, session.KindReport)

	case "cancel":
		subs, err := b.db.GetActiveReportSubscriptions(ctx, chatID)
		if err != nil {
			b.logger.Error("fetching active report subscriptions failed", zap.Int64("chat_id", chatID), zap.Error(err))
			b.send(ctx, chatID, "Произошла ошибка. Попробуйте позже.", nil)
			return
		}
		if len(subs) == 0 {
			b.send(ctx, chatID, "Активных подписок на отчёты не найдено.", nil)
			return
		}
		rows := make([][]InlineKeyboardButton, 0, len(subs))
		for _, s := range subs {
			rows = append(rows, []InlineKeyboardButton{
				{Text: reportSubscriptionBrief(s), CallbackData: fmt.Sprintf("cancel_rpt:%d", s.ID)},
			})
		}
		b.send(ctx, chatID, "Выберите подписку на отчёты, которую нужно отменить:", &InlineKeyboardMarkup{InlineKeyboard: rows})
	}
}

// handleReportPeriodCallback processes "rptperiod:<seconds>" button presses,
// the second step of the report-subscription wizard.
func (b *Bot) handleReportPeriodCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepReportPeriod {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /reports, чтобы начать заново.")
		return
	}

	seconds, err := strconv.Atoi(strings.TrimPrefix(cq.Data, "rptperiod:"))
	if err != nil {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	valid := false
	for _, p := range session.ReportPeriods {
		if p.Seconds == seconds {
			valid = true
			break
		}
	}
	if !valid {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.ReportPeriodSeconds = seconds
	b.answerCallback(ctx, cq.ID, "")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "⏱ Периодичность: "+session.ReportPeriodLabel(seconds), nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepReportPeriod, sess))
}

// handleCancelReportCallback processes "cancel_rpt:<subscription_id>" button
// presses from the /reports cancel selector.
func (b *Bot) handleCancelReportCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	id, err := strconv.Atoi(strings.TrimPrefix(cq.Data, "cancel_rpt:"))
	if err != nil {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	ok, err := b.db.DeactivateReportSubscriptionByID(ctx, chatID, id)
	if err != nil {
		b.logger.Error("deactivate report subscription failed", zap.Int64("chat_id", chatID), zap.Int("subscription_id", id), zap.Error(err))
		b.answerCallback(ctx, cq.ID, "Произошла ошибка при отмене подписки.")
		return
	}
	if !ok {
		b.answerCallback(ctx, cq.ID, "Подписка уже отменена или не найдена.")
		return
	}

	b.answerCallback(ctx, cq.ID, "Подписка на отчёты отменена")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚫 Подписка на отчёты отменена.", nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

// handleDealTypeCallback processes "deal:rent"/"deal:sale" button presses,
// the first step of the subscription wizard.
func (b *Bot) handleDealTypeCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepDealType {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	dealType := strings.TrimPrefix(cq.Data, "deal:")
	if dealType != session.DealTypeRent && dealType != session.DealTypeSale {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.DealType = dealType
	b.answerCallback(ctx, cq.ID, "")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🏷 "+dealTypeLabel(dealType), nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepDealType, sess))
}

// handleRegionCallback processes the region selector: "pg:<page>" switches
// the displayed page, "rgn:<id>" picks a region and advances the wizard.
func (b *Bot) handleRegionCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepRegion {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	switch {
	case strings.HasPrefix(cq.Data, "pg:"):
		page, err := strconv.Atoi(strings.TrimPrefix(cq.Data, "pg:"))
		if err != nil {
			b.answerCallback(ctx, cq.ID, "")
			return
		}
		b.answerCallback(ctx, cq.ID, "")
		text, markup := regionPageContent(sess, page)
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}

	case strings.HasPrefix(cq.Data, "rgn:"):
		id, err := strconv.Atoi(strings.TrimPrefix(cq.Data, "rgn:"))
		if err != nil {
			b.answerCallback(ctx, cq.ID, "")
			return
		}
		sess.Region = id
		b.answerCallback(ctx, cq.ID, "")
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "📍 Регион: "+regionName(id), nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.advance(ctx, chatID, sess, nextStep(session.StepRegion, sess))
	}
}

// handleLocationFilterTypeCallback processes "locf:<type>" button presses,
// the location-filter-kind step asked right after region selection. Picking
// "okrugs", "line", "station", or "district" enters the corresponding
// multi-select sub-step; "any" advances straight past the location filter
// without narrowing by station.
func (b *Bot) handleLocationFilterTypeCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepLocationFilterType {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	filterType := strings.TrimPrefix(cq.Data, "locf:")
	label, ok := locationFilterTypeLabels[filterType]
	if !ok {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.LocationFilterType = filterType
	b.answerCallback(ctx, cq.ID, "")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "📍 Фильтр местоположения: "+label, nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}

	switch filterType {
	case session.LocationFilterOkrugs:
		b.advance(ctx, chatID, sess, session.StepOkrugSelect)
		return
	case session.LocationFilterLine:
		b.advance(ctx, chatID, sess, session.StepLineSelect)
		return
	case session.LocationFilterStation:
		b.advance(ctx, chatID, sess, session.StepStationSelect)
		return
	case session.LocationFilterDistrict:
		b.advance(ctx, chatID, sess, session.StepDistrictSelect)
		return
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepLocationFilterType, sess))
}

// handleOkrugCallback processes the okrug multi-select: "okr:<index>" toggles
// an okrug on/off, "okr:done" finalizes the union of stations and advances.
func (b *Bot) handleOkrugCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepOkrugSelect {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	data := strings.TrimPrefix(cq.Data, "okr:")
	if data == "done" {
		sess.MetroStations = session.StationsForOkrugs(sess.SelectedOkrugs)
		b.answerCallback(ctx, cq.ID, "")
		label := "Не выбраны"
		if len(sess.SelectedOkrugs) > 0 {
			label = strings.Join(sess.SelectedOkrugs, ", ")
		}
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚇 Округа: "+label, nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.advance(ctx, chatID, sess, nextStep(session.StepLocationFilterType, sess))
		return
	}

	idx, err := strconv.Atoi(data)
	if err != nil || idx < 0 || idx >= len(session.Okrugs) {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	name := session.Okrugs[idx].Name

	toggled := false
	for i, n := range sess.SelectedOkrugs {
		if n == name {
			sess.SelectedOkrugs = append(sess.SelectedOkrugs[:i], sess.SelectedOkrugs[i+1:]...)
			toggled = true
			break
		}
	}
	if !toggled {
		sess.SelectedOkrugs = append(sess.SelectedOkrugs, name)
	}
	b.sessions.Set(chatID, sess)

	b.answerCallback(ctx, cq.ID, "")
	text, markup := okrugSelectContent(sess)
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

// handleLineCallback processes the metro line multi-select: "line:<number>"
// toggles a line on/off, "line:done" finalizes the union of stations and
// advances.
func (b *Bot) handleLineCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepLineSelect {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	data := strings.TrimPrefix(cq.Data, "line:")
	if data == "done" {
		sess.MetroStations = session.StationsForLines(sess.SelectedLines)
		b.answerCallback(ctx, cq.ID, "")
		label := "Не выбраны"
		if len(sess.SelectedLines) > 0 {
			labels := make([]string, 0, len(sess.SelectedLines))
			for _, number := range sess.SelectedLines {
				if line, ok := session.LineByNumber(number); ok {
					labels = append(labels, line.Label())
				}
			}
			label = strings.Join(labels, ", ")
		}
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚇 Ветки метро: "+label, nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.advance(ctx, chatID, sess, nextStep(session.StepLocationFilterType, sess))
		return
	}

	if _, ok := session.LineByNumber(data); !ok {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	toggled := false
	for i, n := range sess.SelectedLines {
		if n == data {
			sess.SelectedLines = append(sess.SelectedLines[:i], sess.SelectedLines[i+1:]...)
			toggled = true
			break
		}
	}
	if !toggled {
		sess.SelectedLines = append(sess.SelectedLines, data)
	}
	b.sessions.Set(chatID, sess)

	b.answerCallback(ctx, cq.ID, "")
	text, markup := lineSelectContent(sess)
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

// handleStationCallback processes the paginated station multi-select:
// "stn:pg:<page>" switches the displayed page (keeping selections),
// "stn:s:<index>" toggles the station at that index into session.AllStations,
// "stn:done" finalizes the selection and advances.
func (b *Bot) handleStationCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepStationSelect {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	data := strings.TrimPrefix(cq.Data, "stn:")

	if data == "done" {
		sess.MetroStations = append([]string(nil), sess.SelectedStations...)
		b.answerCallback(ctx, cq.ID, "")
		label := "Не выбраны"
		if len(sess.SelectedStations) > 0 {
			label = strings.Join(sess.SelectedStations, ", ")
		}
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚇 Станции метро: "+label, nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.advance(ctx, chatID, sess, nextStep(session.StepLocationFilterType, sess))
		return
	}

	if strings.HasPrefix(data, "pg:") {
		page, err := strconv.Atoi(strings.TrimPrefix(data, "pg:"))
		if err != nil {
			b.answerCallback(ctx, cq.ID, "")
			return
		}
		b.answerCallback(ctx, cq.ID, "")
		text, markup := stationSelectContent(sess, page)
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		return
	}

	if !strings.HasPrefix(data, "s:") {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(data, "s:"))
	if err != nil || idx < 0 || idx >= len(session.AllStations) {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	name := session.AllStations[idx]

	toggled := false
	for i, n := range sess.SelectedStations {
		if n == name {
			sess.SelectedStations = append(sess.SelectedStations[:i], sess.SelectedStations[i+1:]...)
			toggled = true
			break
		}
	}
	if !toggled {
		sess.SelectedStations = append(sess.SelectedStations, name)
	}
	b.sessions.Set(chatID, sess)

	b.answerCallback(ctx, cq.ID, "")
	page := idx / stationsPerPage
	text, markup := stationSelectContent(sess, page)
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

// handleDistrictCallback processes the paginated district multi-select:
// "dst:pg:<page>" switches the displayed page (keeping selections),
// "dst:d:<index>" toggles the district at that index into session.Districts,
// "dst:done" finalizes the union of stations and advances.
func (b *Bot) handleDistrictCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepDistrictSelect {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	data := strings.TrimPrefix(cq.Data, "dst:")

	if data == "done" {
		sess.MetroStations = session.StationsForDistricts(sess.SelectedDistricts)
		b.answerCallback(ctx, cq.ID, "")
		label := "Не выбраны"
		if len(sess.SelectedDistricts) > 0 {
			label = strings.Join(sess.SelectedDistricts, ", ")
		}
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚇 Районы: "+label, nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.advance(ctx, chatID, sess, nextStep(session.StepLocationFilterType, sess))
		return
	}

	if strings.HasPrefix(data, "pg:") {
		page, err := strconv.Atoi(strings.TrimPrefix(data, "pg:"))
		if err != nil {
			b.answerCallback(ctx, cq.ID, "")
			return
		}
		b.answerCallback(ctx, cq.ID, "")
		text, markup := districtSelectContent(sess, page)
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		return
	}

	if !strings.HasPrefix(data, "d:") {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(data, "d:"))
	if err != nil || idx < 0 || idx >= len(session.Districts) {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	name := session.Districts[idx].Name

	toggled := false
	for i, n := range sess.SelectedDistricts {
		if n == name {
			sess.SelectedDistricts = append(sess.SelectedDistricts[:i], sess.SelectedDistricts[i+1:]...)
			toggled = true
			break
		}
	}
	if !toggled {
		sess.SelectedDistricts = append(sess.SelectedDistricts, name)
	}
	b.sessions.Set(chatID, sess)

	b.answerCallback(ctx, cq.ID, "")
	page := idx / districtsPerPage
	text, markup := districtSelectContent(sess, page)
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, text, markup); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

// handleFilterModeCallback processes "fm:basic"/"fm:extended" button presses.
func (b *Bot) handleFilterModeCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepFilterMode {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	mode := strings.TrimPrefix(cq.Data, "fm:")
	if mode != session.FilterModeBasic && mode != session.FilterModeExtended {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.FilterMode = mode
	b.answerCallback(ctx, cq.ID, "")
	label := "Базовые фильтры"
	if mode == session.FilterModeExtended {
		label = "Расширенные фильтры"
	}
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "⚙️ "+label, nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepFilterMode, sess))
}

// handleScoringModeCallback processes "scmode:default"/"scmode:custom" button
// presses, asked right before the min-score question.
func (b *Bot) handleScoringModeCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepScoringMode {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	mode := strings.TrimPrefix(cq.Data, "scmode:")
	if mode != session.ScoringModeDefault && mode != session.ScoringModeCustom && mode != session.ScoringModePriority {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	if mode == session.ScoringModePriority && sess.Region != moscowRegionID {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.ScoringMode = mode
	b.answerCallback(ctx, cq.ID, "")
	label := "Скоринг по умолчанию"
	switch mode {
	case session.ScoringModeCustom:
		label = "Свои параметры скоринга"
	case session.ScoringModePriority:
		label = "Приоритетные станции"
	}
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🎯 "+label, nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepScoringMode, sess))
}

// handleBoolFilterCallback processes "bool:<key>:1"/"bool:<key>:0" button
// presses shared by all "обязательно"/"не обязательно" extended filters.
func (b *Bot) handleBoolFilterCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	parts := strings.SplitN(strings.TrimPrefix(cq.Data, "bool:"), ":", 2)
	if len(parts) != 2 {
		b.answerCallback(ctx, cq.ID, "")
		return
	}
	meta, ok := boolFilterByKey(parts[0])
	if !ok {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != meta.step {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	required := parts[1] == "1"
	setBoolFilter(sess, parts[0], required)

	b.answerCallback(ctx, cq.ID, "")
	choice := "Не обязательно"
	if required {
		choice = "Обязательно"
	}
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, meta.label+": "+choice, nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(meta.step, sess))
}

func setBoolFilter(sess *session.Session, key string, value bool) {
	switch key {
	case "children":
		sess.ChildrenRequired = value
	case "pets":
		sess.PetsRequired = value
	case "dishwasher":
		sess.DishwasherRequired = value
	case "conditioner":
		sess.ConditionerRequired = value
	case "balcony":
		sess.BalconyRequired = value
	}
}

// handleRenovationCallback processes "renov:<level>" button presses.
func (b *Bot) handleRenovationCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepMinRenovation {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	level := strings.TrimPrefix(cq.Data, "renov:")
	if _, ok := renovationLabels[level]; !ok {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.MinRenovation = level
	b.answerCallback(ctx, cq.ID, "")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🛠 Ремонт не хуже: "+renovationLabels[level], nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepMinRenovation, sess))
}

// handleBathroomCallback processes "bath:<type>" button presses.
func (b *Bot) handleBathroomCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	sess := b.sessions.Get(chatID)
	if sess == nil || sess.Step != session.StepBathroomType {
		b.answerCallback(ctx, cq.ID, "Сессия устарела. Отправьте /subscript, чтобы начать заново.")
		return
	}

	bathType := strings.TrimPrefix(cq.Data, "bath:")
	if _, ok := bathroomLabels[bathType]; !ok {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.BathroomType = bathType
	b.answerCallback(ctx, cq.ID, "")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚿 Санузел: "+bathroomLabels[bathType], nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
	b.advance(ctx, chatID, sess, nextStep(session.StepBathroomType, sess))
}

// handleCancelCallback processes "cancel:<subscription_id>" button presses
// from the /cancel selector.
func (b *Bot) handleCancelCallback(ctx context.Context, chatID int64, cq *CallbackQuery) {
	id, err := strconv.Atoi(strings.TrimPrefix(cq.Data, "cancel:"))
	if err != nil {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	ok, err := b.db.DeactivateSubscriptionByID(ctx, chatID, id)
	if err != nil {
		b.logger.Error("deactivate subscription failed", zap.Int64("chat_id", chatID), zap.Int("subscription_id", id), zap.Error(err))
		b.answerCallback(ctx, cq.ID, "Произошла ошибка при отмене подписки.")
		return
	}
	if !ok {
		b.answerCallback(ctx, cq.ID, "Подписка уже отменена или не найдена.")
		return
	}

	b.answerCallback(ctx, cq.ID, "Подписка отменена")
	if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "🚫 Подписка отменена. Вы больше не будете получать по ней уведомления.", nil); err != nil {
		b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

func (b *Bot) answerCallback(ctx context.Context, callbackQueryID, text string) {
	if err := b.client.AnswerCallbackQuery(ctx, callbackQueryID, text); err != nil {
		b.logger.Warn("answer callback query failed", zap.Error(err))
	}
}

// sendDealTypeQuestion sends the first wizard step: rent vs. sale.
func (b *Bot) sendDealTypeQuestion(ctx context.Context, chatID int64, kind string) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "🏠 Аренда", CallbackData: "deal:" + session.DealTypeRent}},
		{{Text: "💰 Продажа", CallbackData: "deal:" + session.DealTypeSale}},
	}}
	sess := &session.Session{Kind: kind}
	text := fmt.Sprintf("Шаг %d/%d — Что вас интересует?", stepNumber(session.StepDealType, sess), totalSteps(sess))
	b.send(ctx, chatID, text, markup)
}

// sendReportPeriodQuestion asks how often to send the periodic report.
func (b *Bot) sendReportPeriodQuestion(ctx context.Context, chatID int64, sess *session.Session) {
	rows := make([][]InlineKeyboardButton, 0, len(session.ReportPeriods))
	for _, p := range session.ReportPeriods {
		rows = append(rows, []InlineKeyboardButton{
			{Text: p.Label, CallbackData: fmt.Sprintf("rptperiod:%d", p.Seconds)},
		})
	}
	text := fmt.Sprintf("Шаг %d/%d — Как часто присылать отчёт?",
		stepNumber(session.StepReportPeriod, sess), totalSteps(sess))
	b.send(ctx, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

// sendRegionPage sends a new message showing the region selector at the given page.
func (b *Bot) sendRegionPage(ctx context.Context, chatID int64, sess *session.Session, page int) {
	text, markup := regionPageContent(sess, page)
	b.send(ctx, chatID, text, markup)
}

// regionPageContent renders the region selector text and inline keyboard for
// a 0-indexed page of session.Regions.
func regionPageContent(sess *session.Session, page int) (string, *InlineKeyboardMarkup) {
	totalPages := (len(session.Regions) + regionsPerPage - 1) / regionsPerPage
	if page < 0 {
		page = 0
	}
	if page > totalPages-1 {
		page = totalPages - 1
	}

	start := page * regionsPerPage
	end := start + regionsPerPage
	if end > len(session.Regions) {
		end = len(session.Regions)
	}

	rows := make([][]InlineKeyboardButton, 0, regionsPerPage+1)
	for _, r := range session.Regions[start:end] {
		rows = append(rows, []InlineKeyboardButton{
			{Text: r.Name, CallbackData: fmt.Sprintf("rgn:%d", r.ID)},
		})
	}

	var navRow []InlineKeyboardButton
	if page > 0 {
		navRow = append(navRow, InlineKeyboardButton{Text: "◀ Назад", CallbackData: fmt.Sprintf("pg:%d", page-1)})
	}
	if page < totalPages-1 {
		navRow = append(navRow, InlineKeyboardButton{Text: "Далее ▶", CallbackData: fmt.Sprintf("pg:%d", page+1)})
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}

	text := fmt.Sprintf("Шаг %d/%d — Выберите регион (страница %d/%d):",
		stepNumber(session.StepRegion, sess), totalSteps(sess), page+1, totalPages)
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendLocationFilterTypeQuestion asks which kind of metro-based location
// filter (if any) the user wants to narrow their search by.
func (b *Bot) sendLocationFilterTypeQuestion(ctx context.Context, chatID int64, sess *session.Session) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "Округа", CallbackData: "locf:" + session.LocationFilterOkrugs}},
		{{Text: "Районы", CallbackData: "locf:" + session.LocationFilterDistrict}},
		{{Text: "Станции метро", CallbackData: "locf:" + session.LocationFilterStation}},
		{{Text: "Ветки метро", CallbackData: "locf:" + session.LocationFilterLine}},
		{{Text: "Не важно", CallbackData: "locf:" + session.LocationFilterAny}},
	}}
	text := fmt.Sprintf("Шаг %d/%d — Какой фильтр местоположения вас интересует?",
		stepNumber(session.StepLocationFilterType, sess), totalSteps(sess))
	b.send(ctx, chatID, text, markup)
}

// locationFilterTypeLabels maps a stored LocationFilterType to its
// human-readable Russian label.
var locationFilterTypeLabels = map[string]string{
	session.LocationFilterOkrugs:   "Округа",
	session.LocationFilterDistrict: "Районы",
	session.LocationFilterStation:  "Станции метро",
	session.LocationFilterLine:     "Ветки метро",
	session.LocationFilterAny:      "Не важно",
}

// okrugSelectContent renders the multi-select okrug keyboard, marking
// currently selected okrugs with a checkmark.
func okrugSelectContent(sess *session.Session) (string, *InlineKeyboardMarkup) {
	selected := make(map[string]bool, len(sess.SelectedOkrugs))
	for _, name := range sess.SelectedOkrugs {
		selected[name] = true
	}

	rows := make([][]InlineKeyboardButton, 0, len(session.Okrugs)+1)
	for i, o := range session.Okrugs {
		label := o.Name
		if selected[o.Name] {
			label = "✅ " + label
		}
		rows = append(rows, []InlineKeyboardButton{
			{Text: label, CallbackData: fmt.Sprintf("okr:%d", i)},
		})
	}
	rows = append(rows, []InlineKeyboardButton{{Text: "Готово ▶", CallbackData: "okr:done"}})

	text := "🚇 Выберите один или несколько округов (можно отметить несколько), затем нажмите «Готово».\n\n" +
		"Квартира подойдёт, если хотя бы одна из её станций метро относится к выбранным округам."
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendOkrugSelect sends a new message with the okrug multi-select keyboard.
func (b *Bot) sendOkrugSelect(ctx context.Context, chatID int64, sess *session.Session) {
	text, markup := okrugSelectContent(sess)
	b.send(ctx, chatID, text, markup)
}

// lineSelectContent renders the multi-select metro line keyboard, marking
// currently selected lines with a checkmark. Each button shows the line's
// emoji (if any), name, and number/code in parentheses.
func lineSelectContent(sess *session.Session) (string, *InlineKeyboardMarkup) {
	selected := make(map[string]bool, len(sess.SelectedLines))
	for _, number := range sess.SelectedLines {
		selected[number] = true
	}

	rows := make([][]InlineKeyboardButton, 0, len(session.Lines)+1)
	for _, l := range session.Lines {
		label := l.Label()
		if selected[l.Number] {
			label = "✅ " + label
		}
		rows = append(rows, []InlineKeyboardButton{
			{Text: label, CallbackData: "line:" + l.Number},
		})
	}
	rows = append(rows, []InlineKeyboardButton{{Text: "Готово ▶", CallbackData: "line:done"}})

	text := "🚇 Выберите одну или несколько веток метро (можно отметить несколько), затем нажмите «Готово».\n\n" +
		"Квартира подойдёт, если хотя бы одна из её станций метро относится к выбранным веткам."
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendLineSelect sends a new message with the metro line multi-select keyboard.
func (b *Bot) sendLineSelect(ctx context.Context, chatID int64, sess *session.Session) {
	text, markup := lineSelectContent(sess)
	b.send(ctx, chatID, text, markup)
}

// stationSelectContent renders one page (stationsPerPage stations, in
// session.AllStations' alphabetical order) of the multi-select station
// keyboard, marking currently selected stations with a checkmark and adding
// pagination and a "Готово" row.
func stationSelectContent(sess *session.Session, page int) (string, *InlineKeyboardMarkup) {
	totalPages := (len(session.AllStations) + stationsPerPage - 1) / stationsPerPage
	if page < 0 {
		page = 0
	}
	if page > totalPages-1 {
		page = totalPages - 1
	}

	start := page * stationsPerPage
	end := start + stationsPerPage
	if end > len(session.AllStations) {
		end = len(session.AllStations)
	}

	selected := make(map[string]bool, len(sess.SelectedStations))
	for _, name := range sess.SelectedStations {
		selected[name] = true
	}

	rows := make([][]InlineKeyboardButton, 0, stationsPerPage+2)
	for i := start; i < end; i++ {
		name := session.AllStations[i]
		label := name
		if selected[name] {
			label = "✅ " + label
		}
		rows = append(rows, []InlineKeyboardButton{
			{Text: label, CallbackData: fmt.Sprintf("stn:s:%d", i)},
		})
	}

	var navRow []InlineKeyboardButton
	if page > 0 {
		navRow = append(navRow, InlineKeyboardButton{Text: "◀ Назад", CallbackData: fmt.Sprintf("stn:pg:%d", page-1)})
	}
	if page < totalPages-1 {
		navRow = append(navRow, InlineKeyboardButton{Text: "Далее ▶", CallbackData: fmt.Sprintf("stn:pg:%d", page+1)})
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}
	rows = append(rows, []InlineKeyboardButton{{Text: "Готово ▶", CallbackData: "stn:done"}})

	text := fmt.Sprintf("🚇 Выберите одну или несколько станций метро (страница %d/%d), затем нажмите «Готово».\n\n"+
		"Квартира подойдёт, если хотя бы одна из её станций метро совпадёт с выбранными.",
		page+1, totalPages)
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendStationSelect sends a new message with the station multi-select keyboard.
func (b *Bot) sendStationSelect(ctx context.Context, chatID int64, sess *session.Session, page int) {
	text, markup := stationSelectContent(sess, page)
	b.send(ctx, chatID, text, markup)
}

// districtSelectContent renders one page (districtsPerPage districts) of the
// multi-select district keyboard, marking currently selected districts with
// a checkmark and adding pagination and a "Готово" row.
func districtSelectContent(sess *session.Session, page int) (string, *InlineKeyboardMarkup) {
	totalPages := (len(session.Districts) + districtsPerPage - 1) / districtsPerPage
	if page < 0 {
		page = 0
	}
	if page > totalPages-1 {
		page = totalPages - 1
	}

	start := page * districtsPerPage
	end := start + districtsPerPage
	if end > len(session.Districts) {
		end = len(session.Districts)
	}

	selected := make(map[string]bool, len(sess.SelectedDistricts))
	for _, name := range sess.SelectedDistricts {
		selected[name] = true
	}

	rows := make([][]InlineKeyboardButton, 0, districtsPerPage+2)
	for i := start; i < end; i++ {
		d := session.Districts[i]
		label := d.Name
		if selected[d.Name] {
			label = "✅ " + label
		}
		rows = append(rows, []InlineKeyboardButton{
			{Text: label, CallbackData: fmt.Sprintf("dst:d:%d", i)},
		})
	}

	var navRow []InlineKeyboardButton
	if page > 0 {
		navRow = append(navRow, InlineKeyboardButton{Text: "◀ Назад", CallbackData: fmt.Sprintf("dst:pg:%d", page-1)})
	}
	if page < totalPages-1 {
		navRow = append(navRow, InlineKeyboardButton{Text: "Далее ▶", CallbackData: fmt.Sprintf("dst:pg:%d", page+1)})
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}
	rows = append(rows, []InlineKeyboardButton{{Text: "Готово ▶", CallbackData: "dst:done"}})

	text := fmt.Sprintf("🚇 Выберите один или несколько районов (страница %d/%d), затем нажмите «Готово».\n\n"+
		"Квартира подойдёт, если хотя бы одна из её станций метро относится к выбранным районам.",
		page+1, totalPages)
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendDistrictSelect sends a new message with the district multi-select keyboard.
func (b *Bot) sendDistrictSelect(ctx context.Context, chatID int64, sess *session.Session, page int) {
	text, markup := districtSelectContent(sess, page)
	b.send(ctx, chatID, text, markup)
}

// sendFilterModeQuestion asks whether to use basic or extended filters.
func (b *Bot) sendFilterModeQuestion(ctx context.Context, chatID int64, sess *session.Session) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "Базовые фильтры", CallbackData: "fm:" + session.FilterModeBasic}},
		{{Text: "Расширенные фильтры", CallbackData: "fm:" + session.FilterModeExtended}},
	}}
	text := fmt.Sprintf("Шаг %d/%d — Какие фильтры настроить?\n\n"+
		"Базовые — цена, площадь, комнаты, мин. скор.\n"+
		"Расширенные — те же фильтры плюс доп. параметры (метро, этаж, ремонт и т.д.).",
		stepNumber(session.StepFilterMode, sess), totalSteps(sess))
	b.send(ctx, chatID, text, markup)
}

// sendScoringModeQuestion asks whether to use default scoring or a custom
// weighted formula, right before the min-score question. Only reachable for
// KindSubscription — report subscriptions always use default scoring.
func (b *Bot) sendScoringModeQuestion(ctx context.Context, chatID int64, sess *session.Session) {
	rows := [][]InlineKeyboardButton{
		{{Text: "Скоринг по умолчанию", CallbackData: "scmode:" + session.ScoringModeDefault}},
		{{Text: "Настроить параметры скоринга", CallbackData: "scmode:" + session.ScoringModeCustom}},
	}
	body := "По умолчанию — стандартная формула.\n" +
		"Свои параметры — вы укажете, сколько готовы платить за каждый критерий (20 вопросов)."
	if sess.Region == moscowRegionID {
		rows = append(rows, []InlineKeyboardButton{
			{Text: "Уточнить приоритетные станции", CallbackData: "scmode:" + session.ScoringModePriority},
		})
		body += "\nУточнить приоритетные станции — стандартная формула, но вы назовёте станции метро, " +
			"которые вам важны, и увидите пересчитанный топ-10 станций с учётом этого."
	}
	text := fmt.Sprintf("Шаг %d/%d — Как рассчитывать скор квартиры?\n\n%s",
		stepNumber(session.StepScoringMode, sess), totalSteps(sess), body)
	b.send(ctx, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

// scoringQuestionPrefix builds the "Сколько вы готовы ...за" lead-in, which
// differs between rent ("платить в месяц") and sale ("заплатить").
func scoringQuestionPrefix(dealType string) string {
	if dealType == session.DealTypeSale {
		return "Сколько вы готовы заплатить за"
	}
	return "Сколько вы готовы платить в месяц за"
}

// sendScoringQuestion asks one of the custom-scoring questions, showing the
// deal-type-appropriate phrasing and default value and reminding that 0
// means "use the default", numbered separately from the main wizard steps.
func (b *Bot) sendScoringQuestion(ctx context.Context, chatID int64, sess *session.Session, q session.ScoringQuestion) {
	idx, _ := session.ScoringStepIndex(q.Step)
	def := q.Get(session.DefaultScoringParams(sess.DealType))

	var sb strings.Builder
	fmt.Fprintf(&sb, "Скоринг %d/%d — %s %s\n\n", idx+1, len(session.ScoringSteps), scoringQuestionPrefix(sess.DealType), q.Object)
	sb.WriteString("Если ввести 0, будет использовано значение по умолчанию.\n\n")
	fmt.Fprintf(&sb, "По умолчанию: %s ₽", fmtFloatOrAny(def))

	b.send(ctx, chatID, sb.String(), forceReply("Введите число или 0..."))
}

// sendMinScoreQuestion asks for the minimum acceptable flat score, the last
// custom-scoring question — only reachable when the user opted into custom
// scoring; the default formula skips it entirely.
func (b *Bot) sendMinScoreQuestion(ctx context.Context, chatID int64) {
	text := fmt.Sprintf("Скоринг %d/%d — Минимальный скор квартиры?\n\nВведите число или 0, чтобы не ограничивать.",
		len(session.ScoringSteps), len(session.ScoringSteps))
	b.send(ctx, chatID, text, forceReply("Введите число..."))
}

// sendPriorityStationsQuestion asks for the priority station names, the
// single question in the ScoringModePriority flow.
func (b *Bot) sendPriorityStationsQuestion(ctx context.Context, chatID int64) {
	b.send(ctx, chatID,
		"Укажите названия приоритетных станций через пробел или запятую.",
		forceReply("Например: Щёлковская, Белорусская"))
}

// sendBoolFilterQuestion sends an "обязательно"/"не обязательно" choice for
// one of the extended filters described by meta.
func (b *Bot) sendBoolFilterQuestion(ctx context.Context, chatID int64, meta boolFilterMeta) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "Обязательно", CallbackData: "bool:" + meta.key + ":1"},
			{Text: "Не обязательно", CallbackData: "bool:" + meta.key + ":0"},
		},
	}}
	b.send(ctx, chatID, meta.question, markup)
}

// sendRenovationQuestion asks for the minimum acceptable renovation level.
func (b *Bot) sendRenovationQuestion(ctx context.Context, chatID int64) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "Дизайнерский", CallbackData: "renov:" + session.RenovationDesign}},
		{{Text: "Евроремонт", CallbackData: "renov:" + session.RenovationEuro}},
		{{Text: "Косметический", CallbackData: "renov:" + session.RenovationCosmetic}},
		{{Text: "Не важно", CallbackData: "renov:" + session.RenovationAny}},
	}}
	b.send(ctx, chatID, "Доп. фильтр — Ремонт не хуже какого уровня?", markup)
}

// sendBathroomQuestion asks for the desired bathroom type.
func (b *Bot) sendBathroomQuestion(ctx context.Context, chatID int64) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "Раздельный", CallbackData: "bath:" + session.BathroomSeparated}},
		{{Text: "Совмещённый", CallbackData: "bath:" + session.BathroomCombined}},
		{{Text: "Не важно", CallbackData: "bath:" + session.BathroomAny}},
	}}
	b.send(ctx, chatID, "Доп. фильтр — Санузел?", markup)
}

func (b *Bot) finalize(ctx context.Context, chatID int64, sess *session.Session) {
	sub := db.Subscription{
		DealType:         sess.DealType,
		Region:           sess.Region,
		MetroStations:    sess.MetroStations,
		MetroFilterLabel: locationFilterLabel(sess),
		MinPrice:         sess.MinPrice,
		MaxPrice:         sess.MaxPrice,
		MinArea:          sess.MinArea,
		MaxArea:          sess.MaxArea,
		Rooms:            sess.Rooms,
		MinScore:         sess.MinScore,

		MinUndergroundPlace: sess.MinUndergroundPlace,
		MinKitchenArea:      sess.MinKitchenArea,
		MinFloor:            sess.MinFloor,
		MaxFloor:            sess.MaxFloor,
		MinCeilingHeight:    sess.MinCeilingHeight,
		ChildrenRequired:    sess.ChildrenRequired,
		PetsRequired:        sess.PetsRequired,
		DishwasherRequired:  sess.DishwasherRequired,
		ConditionerRequired: sess.ConditionerRequired,
		MinRenovation:       sess.MinRenovation,
		BalconyRequired:     sess.BalconyRequired,
		BathroomType:        sess.BathroomType,
		PriorityStations:    sess.PriorityStations,
	}
	if sess.ScoringMode == session.ScoringModeCustom {
		p := sess.ScoringParams
		sub.ScoringParams = &db.ScoringParams{
			AllArea:            p.AllArea,
			KitchenArea:        p.KitchenArea,
			Pets:               p.Pets,
			Dishwasher:         p.Dishwasher,
			Conditioner:        p.Conditioner,
			Apartments:         p.Apartments,
			TwoRoom:            p.TwoRoom,
			ThreeRoom:          p.ThreeRoom,
			FourRoom:           p.FourRoom,
			AdditionalRooms:    p.AdditionalRooms,
			WindowsYard:        p.WindowsYard,
			WindowsStreet:      p.WindowsStreet,
			WindowsBoth:        p.WindowsBoth,
			RenovationDesign:   p.RenovationDesign,
			RenovationEuro:     p.RenovationEuro,
			RenovationCosmetic: p.RenovationCosmetic,
			BathroomSeparated:  p.BathroomSeparated,
			Balcony:            p.Balcony,
			Loggia:             p.Loggia,
			Underground:        p.Underground,
		}
	}

	if sess.Kind == session.KindReport {
		if err := b.db.CreateReportSubscription(ctx, chatID, sub, sess.ReportPeriodSeconds); err != nil {
			b.logger.Error("create report subscription failed", zap.Int64("chat_id", chatID), zap.Error(err))
			b.send(ctx, chatID, "Произошла ошибка при сохранении подписки. Попробуйте позже.", &ReplyKeyboardRemove{RemoveKeyboard: true})
			return
		}

		b.sessions.Delete(chatID)
		metrics.SubscriptionsCreated.Inc()
		b.logger.Info("report subscription created", zap.Int64("chat_id", chatID))

		summary := formatSubscriptionSummary(sub)
		periodLine := fmt.Sprintf("⏱ Периодичность: %s\n\n", session.ReportPeriodLabel(sess.ReportPeriodSeconds))
		b.send(ctx, chatID, "✅ Подписка на отчёты успешно создана!\n\n"+periodLine+summary, &ReplyKeyboardRemove{RemoveKeyboard: true})
		return
	}

	if err := b.db.CreateSubscription(ctx, chatID, sub); err != nil {
		b.logger.Error("create subscription failed", zap.Int64("chat_id", chatID), zap.Error(err))
		b.send(ctx, chatID, "Произошла ошибка при сохранении подписки. Попробуйте позже.", &ReplyKeyboardRemove{RemoveKeyboard: true})
		return
	}

	b.sessions.Delete(chatID)
	metrics.SubscriptionsCreated.Inc()
	b.logger.Info("subscription created", zap.Int64("chat_id", chatID))

	summary := formatSubscriptionSummary(sub)
	b.send(ctx, chatID, "✅ Подписка успешно создана!\n\n"+summary, &ReplyKeyboardRemove{RemoveKeyboard: true})

	if sess.ScoringMode == session.ScoringModePriority && len(sess.PriorityStations) > 0 {
		b.send(ctx, chatID, priorityTopStationsMessage(sess.PriorityStations), nil)
	}
}

// priorityTopStationsMessage renders the top-10 re-ranked stations after
// boosting priorityStations, shown once right after a subscription with
// ScoringModePriority is created.
func priorityTopStationsMessage(priorityStations []string) string {
	top := metro.TopStations(priorityStations, 10)
	var sb strings.Builder
	sb.WriteString("топ-10 станций с учетом Ваших приоритетов:\n")
	for _, s := range top {
		fmt.Fprintf(&sb, "%s: %.2f\n", s.Name, s.Score)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (b *Bot) sendQuestion(ctx context.Context, chatID int64, sess *session.Session, step session.Step) {
	if body, ok := stepQuestionBody[step]; ok {
		text := fmt.Sprintf("Шаг %d/%d — %s", stepNumber(step, sess), totalSteps(sess), body)
		b.send(ctx, chatID, text, forceReply("Введите ответ..."))
		return
	}
	b.send(ctx, chatID, extendedStepQuestions[step], forceReply("Введите ответ..."))
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, markup interface{}) {
	if err := b.client.SendMessage(ctx, chatID, text, markup); err != nil {
		b.logger.Warn("send message failed", zap.Int64("chat_id", chatID), zap.Error(err))
	}
}

// --- Helpers ---

func forceReply(placeholder string) *ForceReply {
	return &ForceReply{ForceReply: true, InputFieldPlaceholder: placeholder}
}

func parseInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %q", s)
	}
	return v, nil
}

func parseFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return v, nil
}

// parseStationNames splits free-text station names on commas/semicolons if
// present (so multi-word names like "Парк Победы" survive intact), or on
// whitespace otherwise (space-separated single-word names).
func parseStationNames(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var parts []string
	if strings.ContainsAny(s, ",;") {
		parts = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' })
	} else {
		parts = strings.Fields(s)
	}
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			names = append(names, p)
		}
	}
	return names
}

func parseRooms(s string) ([]int32, error) {
	s = strings.TrimSpace(s)
	// Treat "0" as "no filter"
	if s == "0" {
		return nil, nil
	}
	// Replace commas/semicolons with spaces, then split
	f := func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}
	parts := strings.FieldsFunc(s, f)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	var rooms []int32
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 1 {
			return nil, fmt.Errorf("invalid room number: %q", p)
		}
		rooms = append(rooms, int32(v))
	}
	return rooms, nil
}

// locationFilterLabel renders a human-readable description of the metro
// filter the user picked (which okrugs/lines/districts/stations, not just
// the resulting station list), for storage alongside the subscription so
// summaries and the /cancel selector can describe it later. Empty if no
// metro filter was set.
func locationFilterLabel(sess *session.Session) string {
	switch sess.LocationFilterType {
	case session.LocationFilterOkrugs:
		if len(sess.SelectedOkrugs) == 0 {
			return ""
		}
		return "Округа: " + strings.Join(sess.SelectedOkrugs, ", ")
	case session.LocationFilterLine:
		if len(sess.SelectedLines) == 0 {
			return ""
		}
		labels := make([]string, 0, len(sess.SelectedLines))
		for _, number := range sess.SelectedLines {
			if line, ok := session.LineByNumber(number); ok {
				labels = append(labels, line.Label())
			}
		}
		return "Ветки метро: " + strings.Join(labels, ", ")
	case session.LocationFilterDistrict:
		if len(sess.SelectedDistricts) == 0 {
			return ""
		}
		return "Районы: " + strings.Join(sess.SelectedDistricts, ", ")
	case session.LocationFilterStation:
		if len(sess.SelectedStations) == 0 {
			return ""
		}
		return "Станции метро: " + strings.Join(sess.SelectedStations, ", ")
	default:
		return ""
	}
}

func formatSubscriptionSummary(sub db.Subscription) string {
	var sb strings.Builder
	sb.WriteString("🏷 " + dealTypeLabel(sub.DealType) + "\n")
	if name := regionName(sub.Region); name != "" {
		sb.WriteString("📍 Регион: " + name + "\n")
	}
	if sub.MinPrice > 0 || sub.MaxPrice > 0 {
		sb.WriteString(fmt.Sprintf("💰 Цена: %s — %s ₽\n",
			fmtOrAny(sub.MinPrice), fmtOrAny(sub.MaxPrice)))
	}
	if sub.MinArea > 0 || sub.MaxArea > 0 {
		sb.WriteString(fmt.Sprintf("📐 Площадь: %s — %s м²\n",
			fmtFloatOrAny(sub.MinArea), fmtFloatOrAny(sub.MaxArea)))
	}
	if len(sub.Rooms) > 0 {
		parts := make([]string, len(sub.Rooms))
		for i, r := range sub.Rooms {
			parts[i] = strconv.Itoa(int(r))
		}
		sb.WriteString("🚪 Комнат: " + strings.Join(parts, ", ") + "\n")
	}
	if sub.MetroFilterLabel != "" {
		sb.WriteString("🚇 " + sub.MetroFilterLabel + "\n")
	} else if len(sub.MetroStations) > 0 {
		// Fallback for subscriptions created before metro_filter_label existed.
		sb.WriteString("🚇 Станции метро: " + strings.Join(sub.MetroStations, ", ") + "\n")
	}
	if sub.MinScore > 0 {
		sb.WriteString(fmt.Sprintf("⭐ Мин. скор: %d\n", sub.MinScore))
	}
	if sub.ScoringParams != nil {
		sb.WriteString("🎯 Скоринг: свои параметры\n")
	}
	if len(sub.PriorityStations) > 0 {
		sb.WriteString("🎯 Приоритетные станции: " + strings.Join(sub.PriorityStations, ", ") + "\n")
	}

	if sub.MinUndergroundPlace > 0 {
		sb.WriteString(fmt.Sprintf("🚇 Место метро не хуже: %d\n", sub.MinUndergroundPlace))
	}
	if sub.MinKitchenArea > 0 {
		sb.WriteString(fmt.Sprintf("🍳 Мин. площадь кухни: %s м²\n", fmtFloatOrAny(sub.MinKitchenArea)))
	}
	if sub.MinFloor > 0 || sub.MaxFloor > 0 {
		sb.WriteString(fmt.Sprintf("🏢 Этаж: %s — %s\n", fmtOrAny(sub.MinFloor), fmtOrAny(sub.MaxFloor)))
	}
	if sub.MinCeilingHeight > 0 {
		sb.WriteString(fmt.Sprintf("📏 Мин. высота потолков: %s м\n", fmtFloatOrAny(sub.MinCeilingHeight)))
	}
	if sub.ChildrenRequired {
		sb.WriteString("👶 Можно с детьми: обязательно\n")
	}
	if sub.PetsRequired {
		sb.WriteString("🐾 Можно с животными: обязательно\n")
	}
	if sub.DishwasherRequired {
		sb.WriteString("🍽 Посудомойка: обязательно\n")
	}
	if sub.ConditionerRequired {
		sb.WriteString("❄️ Кондиционер: обязательно\n")
	}
	if sub.MinRenovation != "" {
		sb.WriteString("🛠 Ремонт не хуже: " + renovationLabels[sub.MinRenovation] + "\n")
	}
	if sub.BalconyRequired {
		sb.WriteString("🌇 Балкон/лоджия: обязательно\n")
	}
	if sub.BathroomType != "" {
		sb.WriteString("🚿 Санузел: " + bathroomLabels[sub.BathroomType] + "\n")
	}

	if sb.Len() == 0 {
		sb.WriteString("Без фильтров — будут присылаться все объявления.")
	}
	return sb.String()
}

// briefMetroLabel truncates a MetroFilterLabel for use in a one-line brief,
// so a long list of selected stations doesn't blow out the button text.
func briefMetroLabel(label string) string {
	const maxLen = 40
	if label == "" || utf8.RuneCountInString(label) <= maxLen {
		return label
	}
	runes := []rune(label)
	return string(runes[:maxLen]) + "…"
}

// extendedFilterTags renders short tags for whichever extended filters are
// set, shared by subscriptionBrief and reportSubscriptionBrief.
func extendedFilterTags(
	minUndergroundPlace int, minKitchenArea float64, minFloor, maxFloor int, minCeilingHeight float64,
	childrenRequired, petsRequired, dishwasherRequired, conditionerRequired bool,
	minRenovation string, balconyRequired bool, bathroomType string,
) []string {
	var tags []string
	if minUndergroundPlace > 0 {
		tags = append(tags, fmt.Sprintf("метро≥%d", minUndergroundPlace))
	}
	if minKitchenArea > 0 {
		tags = append(tags, fmt.Sprintf("кухня≥%sм²", fmtFloatOrAny(minKitchenArea)))
	}
	if minFloor > 0 || maxFloor > 0 {
		tags = append(tags, fmt.Sprintf("этаж %s-%s", fmtOrAny(minFloor), fmtOrAny(maxFloor)))
	}
	if minCeilingHeight > 0 {
		tags = append(tags, fmt.Sprintf("потолки≥%sм", fmtFloatOrAny(minCeilingHeight)))
	}
	if childrenRequired {
		tags = append(tags, "дети")
	}
	if petsRequired {
		tags = append(tags, "животные")
	}
	if dishwasherRequired {
		tags = append(tags, "посудомойка")
	}
	if conditionerRequired {
		tags = append(tags, "кондиционер")
	}
	if minRenovation != "" {
		tags = append(tags, "ремонт:"+renovationLabels[minRenovation])
	}
	if balconyRequired {
		tags = append(tags, "балкон")
	}
	if bathroomType != "" {
		tags = append(tags, "с/у:"+bathroomLabels[bathroomType])
	}
	return tags
}

// subscriptionBrief renders a short one-line summary of a subscription for
// use as an inline-keyboard button label in the /cancel selector.
func subscriptionBrief(sub db.Subscription) string {
	parts := []string{dealTypeLabel(sub.DealType), regionName(sub.Region)}

	if sub.MinPrice > 0 || sub.MaxPrice > 0 {
		parts = append(parts, fmt.Sprintf("%s-%s₽", fmtOrAny(sub.MinPrice), fmtOrAny(sub.MaxPrice)))
	}
	if len(sub.Rooms) > 0 {
		roomParts := make([]string, len(sub.Rooms))
		for i, r := range sub.Rooms {
			roomParts[i] = strconv.Itoa(int(r))
		}
		parts = append(parts, strings.Join(roomParts, ",")+"к")
	}
	if sub.MinScore > 0 {
		parts = append(parts, fmt.Sprintf("скор≥%d", sub.MinScore))
	}
	if label := briefMetroLabel(sub.MetroFilterLabel); label != "" {
		parts = append(parts, label)
	}
	parts = append(parts, extendedFilterTags(
		sub.MinUndergroundPlace, sub.MinKitchenArea, sub.MinFloor, sub.MaxFloor, sub.MinCeilingHeight,
		sub.ChildrenRequired, sub.PetsRequired, sub.DishwasherRequired, sub.ConditionerRequired,
		sub.MinRenovation, sub.BalconyRequired, sub.BathroomType,
	)...)

	return strings.Join(parts, ", ")
}

// reportSubscriptionBrief renders a short one-line summary of a report
// subscription for use as an inline-keyboard button label in the /reports
// cancel selector.
func reportSubscriptionBrief(sub db.ReportSubscription) string {
	parts := []string{dealTypeLabel(sub.DealType), regionName(sub.Region), session.ReportPeriodLabel(sub.PeriodSeconds)}

	if sub.MinPrice > 0 || sub.MaxPrice > 0 {
		parts = append(parts, fmt.Sprintf("%s-%s₽", fmtOrAny(sub.MinPrice), fmtOrAny(sub.MaxPrice)))
	}
	if len(sub.Rooms) > 0 {
		roomParts := make([]string, len(sub.Rooms))
		for i, r := range sub.Rooms {
			roomParts[i] = strconv.Itoa(int(r))
		}
		parts = append(parts, strings.Join(roomParts, ",")+"к")
	}
	if label := briefMetroLabel(sub.MetroFilterLabel); label != "" {
		parts = append(parts, label)
	}
	parts = append(parts, extendedFilterTags(
		sub.MinUndergroundPlace, sub.MinKitchenArea, sub.MinFloor, sub.MaxFloor, sub.MinCeilingHeight,
		sub.ChildrenRequired, sub.PetsRequired, sub.DishwasherRequired, sub.ConditionerRequired,
		sub.MinRenovation, sub.BalconyRequired, sub.BathroomType,
	)...)

	return strings.Join(parts, ", ")
}

func regionName(regionID int) string {
	for _, r := range session.Regions {
		if r.ID == regionID {
			return r.Name
		}
	}
	return ""
}

func fmtOrAny(v int) string {
	if v == 0 {
		return "любая"
	}
	return strconv.Itoa(v)
}

func fmtFloatOrAny(v float64) string {
	if v == 0 {
		return "любая"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
