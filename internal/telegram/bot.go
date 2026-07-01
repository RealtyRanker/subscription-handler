package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.uber.org/zap"

	"github.com/asmisnik/subscription-handler/internal/db"
	"github.com/asmisnik/subscription-handler/internal/metrics"
	"github.com/asmisnik/subscription-handler/internal/session"
)

const regionsPerPage = 10

// fixedSubscriptionSteps/fixedReportSteps list the main (non-extended,
// non-scoring) wizard steps in order for each wizard kind, used to compute
// "Шаг N/M" numbering. Both happen to total 10: the report wizard swaps
// StepScoringMode (subscription-only) for StepReportPeriod (report-only).
var fixedSubscriptionSteps = []session.Step{
	session.StepDealType, session.StepRegion, session.StepFilterMode,
	session.StepMinPrice, session.StepMaxPrice, session.StepMinArea, session.StepMaxArea,
	session.StepRooms, session.StepScoringMode, session.StepMinScore,
}

var fixedReportSteps = []session.Step{
	session.StepDealType, session.StepReportPeriod, session.StepRegion, session.StepFilterMode,
	session.StepMinPrice, session.StepMaxPrice, session.StepMinArea, session.StepMaxArea,
	session.StepRooms, session.StepMinScore,
}

func fixedSteps(kind string) []session.Step {
	if kind == session.KindReport {
		return fixedReportSteps
	}
	return fixedSubscriptionSteps
}

// stepNumber returns the 1-based position of step in kind's fixed step list.
func stepNumber(step session.Step, kind string) int {
	for i, s := range fixedSteps(kind) {
		if s == step {
			return i + 1
		}
	}
	return 0
}

func totalSteps(kind string) int {
	return len(fixedSteps(kind))
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
	session.StepMinScore: "Минимальный скор квартиры?\n\nВведите число или 0, чтобы не ограничивать.",
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
			return session.StepMinScore
		}
		return session.StepScoringMode
	case session.StepScoringMode:
		if sess.ScoringMode == session.ScoringModeCustom {
			return session.ScoringSteps[0]
		}
		return session.StepMinScore
	case session.StepMinScore:
		if sess.FilterMode != session.FilterModeExtended {
			return session.StepDone
		}
		return session.StepMinUndergroundPlace
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
			return session.StepMinScore
		}
		return session.StepDone
	}
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
	case session.StepFilterMode:
		b.sendFilterModeQuestion(ctx, chatID, sess)
	case session.StepScoringMode:
		b.sendScoringModeQuestion(ctx, chatID, sess)
	case session.StepMinRenovation:
		b.sendRenovationQuestion(ctx, chatID)
	case session.StepBathroomType:
		b.sendBathroomQuestion(ctx, chatID)
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
		session.StepScoringMode,
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
		text, markup := regionPageContent(sess.Kind, page)
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
	if mode != session.ScoringModeDefault && mode != session.ScoringModeCustom {
		b.answerCallback(ctx, cq.ID, "")
		return
	}

	sess.ScoringMode = mode
	b.answerCallback(ctx, cq.ID, "")
	label := "Скоринг по умолчанию"
	if mode == session.ScoringModeCustom {
		label = "Свои параметры скоринга"
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
	text := fmt.Sprintf("Шаг %d/%d — Что вас интересует?", stepNumber(session.StepDealType, kind), totalSteps(kind))
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
		stepNumber(session.StepReportPeriod, sess.Kind), totalSteps(sess.Kind))
	b.send(ctx, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

// sendRegionPage sends a new message showing the region selector at the given page.
func (b *Bot) sendRegionPage(ctx context.Context, chatID int64, sess *session.Session, page int) {
	text, markup := regionPageContent(sess.Kind, page)
	b.send(ctx, chatID, text, markup)
}

// regionPageContent renders the region selector text and inline keyboard for
// a 0-indexed page of session.Regions.
func regionPageContent(kind string, page int) (string, *InlineKeyboardMarkup) {
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
		stepNumber(session.StepRegion, kind), totalSteps(kind), page+1, totalPages)
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
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
		stepNumber(session.StepFilterMode, sess.Kind), totalSteps(sess.Kind))
	b.send(ctx, chatID, text, markup)
}

// sendScoringModeQuestion asks whether to use default scoring or a custom
// weighted formula, right before the min-score question. Only reachable for
// KindSubscription — report subscriptions always use default scoring.
func (b *Bot) sendScoringModeQuestion(ctx context.Context, chatID int64, sess *session.Session) {
	markup := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "Скоринг по умолчанию", CallbackData: "scmode:" + session.ScoringModeDefault}},
		{{Text: "Настроить параметры скоринга", CallbackData: "scmode:" + session.ScoringModeCustom}},
	}}
	text := fmt.Sprintf("Шаг %d/%d — Как рассчитывать скор квартиры?\n\n"+
		"По умолчанию — стандартная формула.\n"+
		"Свои параметры — вы укажете, сколько готовы платить за каждый критерий (20 вопросов).",
		stepNumber(session.StepScoringMode, sess.Kind), totalSteps(sess.Kind))
	b.send(ctx, chatID, text, markup)
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
		DealType: sess.DealType,
		Region:   sess.Region,
		MinPrice: sess.MinPrice,
		MaxPrice: sess.MaxPrice,
		MinArea:  sess.MinArea,
		MaxArea:  sess.MaxArea,
		Rooms:    sess.Rooms,
		MinScore: sess.MinScore,

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
}

func (b *Bot) sendQuestion(ctx context.Context, chatID int64, sess *session.Session, step session.Step) {
	if body, ok := stepQuestionBody[step]; ok {
		text := fmt.Sprintf("Шаг %d/%d — %s", stepNumber(step, sess.Kind), totalSteps(sess.Kind), body)
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
	if sub.MinScore > 0 {
		sb.WriteString(fmt.Sprintf("⭐ Мин. скор: %d\n", sub.MinScore))
	}
	if sub.ScoringParams != nil {
		sb.WriteString("🎯 Скоринг: свои параметры\n")
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
