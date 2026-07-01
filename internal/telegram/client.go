package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const apiBase = "https://api.telegram.org"

type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 40 * time.Second},
	}
}

// --- Incoming types ---

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	From      User   `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
}

type Chat struct {
	ID int64 `json:"id"`
}

// --- API response wrapper ---

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// --- GetUpdates ---

func (c *Client) GetUpdates(ctx context.Context, offset, timeout int) ([]Update, error) {
	url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%s&timeout=%s&allowed_updates=[\"message\",\"callback_query\"]",
		apiBase, c.token, strconv.Itoa(offset), strconv.Itoa(timeout))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram API: %s", result.Description)
	}

	var updates []Update
	if err := json.Unmarshal(result.Result, &updates); err != nil {
		return nil, fmt.Errorf("decoding updates: %w", err)
	}
	return updates, nil
}

// --- SendMessage ---

type sendMessageRequest struct {
	ChatID      int64       `json:"chat_id"`
	Text        string      `json:"text"`
	ReplyMarkup interface{} `json:"reply_markup,omitempty"`
}

type ForceReply struct {
	ForceReply            bool   `json:"force_reply"`
	InputFieldPlaceholder string `json:"input_field_placeholder,omitempty"`
}

type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, markup interface{}) error {
	payload, err := json.Marshal(sendMessageRequest{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: markup,
	})
	if err != nil {
		return err
	}
	return c.call(ctx, "sendMessage", payload)
}

// --- EditMessageText ---

type editMessageTextRequest struct {
	ChatID      int64       `json:"chat_id"`
	MessageID   int         `json:"message_id"`
	Text        string      `json:"text"`
	ReplyMarkup interface{} `json:"reply_markup,omitempty"`
}

func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int, text string, markup interface{}) error {
	payload, err := json.Marshal(editMessageTextRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        text,
		ReplyMarkup: markup,
	})
	if err != nil {
		return err
	}
	return c.call(ctx, "editMessageText", payload)
}

// --- AnswerCallbackQuery ---

type answerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string) error {
	payload, err := json.Marshal(answerCallbackQueryRequest{
		CallbackQueryID: callbackQueryID,
		Text:            text,
	})
	if err != nil {
		return err
	}
	return c.call(ctx, "answerCallbackQuery", payload)
}

func (c *Client) call(ctx context.Context, method string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", apiBase, c.token, method),
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram API: %s", result.Description)
	}
	return nil
}