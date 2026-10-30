package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// TG — минимальный клиент Bot API на stdlib: long polling, сообщения и
// документы. Вебхук не нужен: бот живёт на машине оператора за NAT.
type TG struct {
	api string
	hc  *http.Client
}

func NewTG(token string) *TG {
	return &TG{api: "https://api.telegram.org/bot" + token, hc: &http.Client{Timeout: 75 * time.Second}}
}

type TGUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type TGMessage struct {
	MessageID int64   `json:"message_id"`
	From      *TGUser `json:"from"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

type TGUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *TGMessage `json:"message"`
}

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (t *TG) call(method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := t.hc.Post(t.api+"/"+method, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var env tgEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: %s", method, truncate(string(raw), 200))
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (t *TG) GetUpdates(offset int64, timeoutSec int) ([]TGUpdate, error) {
	var updates []TGUpdate
	err := t.call("getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

func (t *TG) SendMessage(chatID int64, text string) error {
	// HTML-разметку не включаем: в текст попадают чужие username и вывод
	// плагинов, и plain text — это отсутствие целого класса проблем с экранированием.
	return t.call("sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}, nil)
}

func (t *TG) SendDocument(chatID int64, path, caption string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile("document", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	resp, err := t.hc.Post(t.api+"/sendDocument", mw.FormDataContentType(), &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env tgEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("sendDocument: %s", truncate(string(raw), 200))
	}
	if !env.OK {
		return fmt.Errorf("sendDocument: %s", env.Description)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
