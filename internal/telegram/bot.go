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

var stepQuestions = map[session.Step]string{
	session.StepMinPrice: "Шаг 2/7 — Минимальная цена аренды (₽)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxPrice: "Шаг 3/7 — Максимальная цена аренды (₽)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMinArea:  "Шаг 4/7 — Минимальная площадь (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxArea:  "Шаг 5/7 — Максимальная площадь (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepRooms:    "Шаг 6/7 — Количество комнат?\n\nВведите числа через пробел или запятую (например: 2 3 или 1,2,3).\nОтправьте 0, чтобы не фильтровать по комнатам.",
	session.StepMinScore: "Шаг 7/7 — Минимальный скор квартиры?\n\nВведите число или 0, чтобы не ограничивать.",
}

type Bot struct {
	client  *Client
	db      *db.DB
	sessions *session.Store
	logger  *zap.Logger
}

func NewBot(client *Client, database *db.DB, sessions *session.Store, logger *zap.Logger) *Bot {
	return &Bot{
		client:  client,
		db:      database,
		sessions: sessions,
		logger:  logger,
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
		b.sessions.Set(chatID, &session.Session{Step: session.StepRegion})
		b.sendRegionPage(ctx, chatID, 0)

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

	default:
		b.send(ctx, chatID, "Неизвестная команда.\n\nДоступные команды:\n/subscript — создать новую подписку\n/cancel — отменить одну из активных подписок", nil)
	}
}

func (b *Bot) handleAnswer(ctx context.Context, chatID int64, sess *session.Session, text string) {
	var err error

	switch sess.Step {
	case session.StepRegion:
		b.send(ctx, chatID, "Пожалуйста, выберите регион, нажав на кнопку в списке выше.", nil)
		return

	case session.StepMinPrice:
		sess.MinPrice, err = parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.Step = session.StepMaxPrice
		b.sendQuestion(ctx, chatID, session.StepMaxPrice)

	case session.StepMaxPrice:
		sess.MaxPrice, err = parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		sess.Step = session.StepMinArea
		b.sendQuestion(ctx, chatID, session.StepMinArea)

	case session.StepMinArea:
		sess.MinArea, err = parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		sess.Step = session.StepMaxArea
		b.sendQuestion(ctx, chatID, session.StepMaxArea)

	case session.StepMaxArea:
		sess.MaxArea, err = parseFloat(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите число.", forceReply("Введите число..."))
			return
		}
		sess.Step = session.StepRooms
		b.sendQuestion(ctx, chatID, session.StepRooms)

	case session.StepRooms:
		sess.Rooms, err = parseRooms(text)
		if err != nil {
			b.send(ctx, chatID, "Введите числа через пробел или запятую, либо 0.", forceReply("Например: 2 3"))
			return
		}
		sess.Step = session.StepMinScore
		b.sendQuestion(ctx, chatID, session.StepMinScore)

	case session.StepMinScore:
		sess.MinScore, err = parseInt(text)
		if err != nil {
			b.send(ctx, chatID, "Пожалуйста, введите целое число.", forceReply("Введите число..."))
			return
		}
		b.finalize(ctx, chatID, sess)
		return
	}

	b.sessions.Set(chatID, sess)
}

// handleCallbackQuery routes inline-keyboard button presses: "pg:"/"rgn:" for
// the region selector in the subscription wizard, "cancel:" for picking which
// active subscription to cancel.
func (b *Bot) handleCallbackQuery(ctx context.Context, cq *CallbackQuery) {
	if cq.Message == nil {
		return
	}
	chatID := cq.Message.Chat.ID

	switch {
	case strings.HasPrefix(cq.Data, "pg:"), strings.HasPrefix(cq.Data, "rgn:"):
		b.handleRegionCallback(ctx, chatID, cq)
	case strings.HasPrefix(cq.Data, "cancel:"):
		b.handleCancelCallback(ctx, chatID, cq)
	}
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
		text, markup := regionPageContent(page)
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
		sess.Step = session.StepMinPrice
		b.sessions.Set(chatID, sess)
		b.answerCallback(ctx, cq.ID, "")
		if err := b.client.EditMessageText(ctx, chatID, cq.Message.MessageID, "📍 Регион: "+regionName(id), nil); err != nil {
			b.logger.Warn("edit message failed", zap.Int64("chat_id", chatID), zap.Error(err))
		}
		b.sendQuestion(ctx, chatID, session.StepMinPrice)
	}
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

// sendRegionPage sends a new message showing the region selector at the given page.
func (b *Bot) sendRegionPage(ctx context.Context, chatID int64, page int) {
	text, markup := regionPageContent(page)
	b.send(ctx, chatID, text, markup)
}

// regionPageContent renders the region selector text and inline keyboard for
// a 0-indexed page of session.Regions.
func regionPageContent(page int) (string, *InlineKeyboardMarkup) {
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

	text := fmt.Sprintf("Шаг 1/7 — Выберите регион (страница %d/%d):", page+1, totalPages)
	return text, &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (b *Bot) finalize(ctx context.Context, chatID int64, sess *session.Session) {
	sub := db.Subscription{
		Region:   sess.Region,
		MinPrice: sess.MinPrice,
		MaxPrice: sess.MaxPrice,
		MinArea:  sess.MinArea,
		MaxArea:  sess.MaxArea,
		Rooms:    sess.Rooms,
		MinScore: sess.MinScore,
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

func (b *Bot) sendQuestion(ctx context.Context, chatID int64, step session.Step) {
	b.send(ctx, chatID, stepQuestions[step], forceReply("Введите ответ..."))
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
	if sb.Len() == 0 {
		sb.WriteString("Без фильтров — будут присылаться все объявления.")
	}
	return sb.String()
}

// subscriptionBrief renders a short one-line summary of a subscription for
// use as an inline-keyboard button label in the /cancel selector.
func subscriptionBrief(sub db.Subscription) string {
	parts := []string{regionName(sub.Region)}

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