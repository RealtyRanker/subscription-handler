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

var stepQuestions = map[session.Step]string{
	session.StepMinPrice: "Шаг 1/6 — Минимальная цена аренды (₽)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxPrice: "Шаг 2/6 — Максимальная цена аренды (₽)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMinArea:  "Шаг 3/6 — Минимальная площадь (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepMaxArea:  "Шаг 4/6 — Максимальная площадь (м²)?\n\nВведите число или 0, чтобы не ограничивать.",
	session.StepRooms:    "Шаг 5/6 — Количество комнат?\n\nВведите числа через пробел или запятую (например: 2 3 или 1,2,3).\nОтправьте 0, чтобы не фильтровать по комнатам.",
	session.StepMinScore: "Шаг 6/6 — Минимальный скор квартиры?\n\nВведите число или 0, чтобы не ограничивать.",
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
		b.sessions.Set(chatID, &session.Session{Step: session.StepMinPrice})
		b.sendQuestion(ctx, chatID, session.StepMinPrice)

	case "/cancel":
		b.sessions.Delete(chatID)
		b.send(ctx, chatID, "Создание подписки отменено.", &ReplyKeyboardRemove{RemoveKeyboard: true})

	default:
		b.send(ctx, chatID, "Неизвестная команда.\n\nДоступные команды:\n/subscript — создать или обновить подписку\n/cancel — отменить", nil)
	}
}

func (b *Bot) handleAnswer(ctx context.Context, chatID int64, sess *session.Session, text string) {
	var err error

	switch sess.Step {
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

func (b *Bot) finalize(ctx context.Context, chatID int64, sess *session.Session) {
	sub := db.Subscription{
		MinPrice: sess.MinPrice,
		MaxPrice: sess.MaxPrice,
		MinArea:  sess.MinArea,
		MaxArea:  sess.MaxArea,
		Rooms:    sess.Rooms,
		MinScore: sess.MinScore,
	}

	if err := b.db.UpsertSubscription(ctx, chatID, sub); err != nil {
		b.logger.Error("upsert subscription failed", zap.Int64("chat_id", chatID), zap.Error(err))
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