package bot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Order — один заказ досье. Хранилище — append-only JSONL, как журнал
// движка: последнее состояние заказа перекрывает предыдущее, а история
// «скан → счёт → оплата → выдача» остаётся на диске целиком.
type Order struct {
	ID           string    `json:"id"`
	UserID       int64     `json:"user_id"`
	ChatID       int64     `json:"chat_id"`
	Kind         string    `json:"kind"` // username | email
	Target       string    `json:"target"`
	Status       string    `json:"status"`
	QuickSummary string    `json:"quick_summary,omitempty"`
	InvoiceID    int64     `json:"invoice_id,omitempty"`
	PayURL       string    `json:"pay_url,omitempty"`
	Amount       string    `json:"amount,omitempty"`
	RunDir       string    `json:"run_dir,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const (
	StatusScanned         = "scanned"
	StatusAwaitingPayment = "awaiting_payment"
	StatusPaid            = "paid"
	StatusRunning         = "running"
	StatusDelivered       = "delivered"
	StatusFailed          = "failed"
	StatusExpired         = "expired"
)

type Store struct {
	mu       sync.Mutex
	dir      string
	byID     map[string]*Order
	lastFree map[int64]time.Time
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, byID: map[string]*Order{}, lastFree: map[int64]time.Time{}}
	raw, err := os.ReadFile(filepath.Join(dir, "orders.jsonl"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var o Order
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			return nil, fmt.Errorf("orders.jsonl строка %d: %w", i+1, err)
		}
		cp := o
		s.byID[o.ID] = &cp
	}
	return s, nil
}

func (s *Store) Get(id string) (*Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	cp := *o
	return &cp, true
}

func (s *Store) Save(o *Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o.UpdatedAt = time.Now().UTC()
	cp := *o
	s.byID[o.ID] = &cp
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "orders.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func (s *Store) AwaitingPayment() []*Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Order
	for _, o := range s.byID {
		if o.Status == StatusAwaitingPayment {
			cp := *o
			out = append(out, &cp)
		}
	}
	return out
}

// FreeAllowed — кулдаун бесплатного скана: без него бот превращается
// в бесплатный публичный сканер, а платить перестают вообще.
func (s *Store) FreeAllowed(userID int64, cooldown time.Duration) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastFree[userID]
	if !ok {
		return 0, true
	}
	wait := cooldown - time.Since(last)
	return wait, wait <= 0
}

func (s *Store) MarkFree(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFree[userID] = time.Now()
}

func NewOrderID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
