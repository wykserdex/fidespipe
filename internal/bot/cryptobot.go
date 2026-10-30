package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Crypto — клиент Crypto Bot (pay.crypt.bot). Выбран потому, что оплата
// приходит в USDT на крипто-кошелёк оператора: ни эквайринга, ни 18+,
// ни привязки карты. Проверка оплаты — по getInvoices, вебхук не нужен.
type Crypto struct {
	token string
	hc    *http.Client
}

func NewCrypto(token string) *Crypto {
	return &Crypto{token: token, hc: &http.Client{Timeout: 30 * time.Second}}
}

type Invoice struct {
	ID     int64  `json:"invoice_id"`
	PayURL string `json:"pay_url"`
	Status string `json:"status"`
}

type cryptoEnvelope struct {
	OK    bool `json:"ok"`
	Error *struct {
		Code int    `json:"code"`
		Name string `json:"name"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

func (c *Crypto) CreateInvoice(amount, description, payload string) (*Invoice, error) {
	body, err := json.Marshal(map[string]any{
		"asset":           "USDT",
		"amount":          amount,
		"description":     description,
		"payload":         payload,
		"expires_in":      1800,
		"allow_comments":  false,
		"allow_anonymous": true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "https://pay.crypt.bot/api/createInvoice", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Crypto-Pay-Api-Token", c.token)
	var result struct {
		Invoice
	}
	if err := c.do(req, &result); err != nil {
		return nil, err
	}
	return &result.Invoice, nil
}

func (c *Crypto) InvoiceStatus(id int64) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://pay.crypt.bot/api/getInvoices?invoice_ids="+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Crypto-Pay-Api-Token", c.token)
	var result struct {
		Items []Invoice `json:"items"`
	}
	if err := c.do(req, &result); err != nil {
		return "", err
	}
	if len(result.Items) == 0 {
		return "", fmt.Errorf("invoice %d не найден", id)
	}
	return result.Items[0].Status, nil
}

func (c *Crypto) do(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env cryptoEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("cryptobot: %s", truncate(string(raw), 200))
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("cryptobot: %s (%d)", env.Error.Name, env.Error.Code)
		}
		return fmt.Errorf("cryptobot: запрос отклонён")
	}
	return json.Unmarshal(env.Result, out)
}
