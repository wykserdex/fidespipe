package bot

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)
	emailRe    = regexp.MustCompile(`^[^@\s/]{1,64}@[^@\s/]{1,190}\.[^@\s/]{2,24}$`)
)

type Bot struct {
	cfg    Config
	tg     *TG
	cb     *Crypto
	store  *Store
	runner *Runner

	mu     sync.Mutex
	active map[int64]bool
}

func New(cfg Config) (*Bot, error) {
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	var cb *Crypto
	if cfg.CryptoToken != "" {
		cb = NewCrypto(cfg.CryptoToken)
	}
	return &Bot{
		cfg:   cfg,
		tg:    NewTG(cfg.TelegramToken),
		cb:    cb,
		store: store,
		runner: NewRunner(cfg.FidespipeBin, cfg.RepoRoot, filepath.Join(cfg.DataDir, "runs"),
			ScriptsDir(cfg.PythonBin), cfg.MaxConcurrentRuns),
		active: map[int64]bool{},
	}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	if b.cb != nil {
		go b.pollInvoices(ctx)
	} else {
		log.Printf("cryptobot_token не задан: /buy будет честно отвечать, что платежи не настроены")
	}
	var offset int64
	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := b.tg.GetUpdates(offset, 50)
		if err != nil {
			log.Printf("getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.From == nil {
				continue
			}
			go b.handle(ctx, u.Message)
		}
	}
}

func (b *Bot) handle(ctx context.Context, m *TGMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic в handle: %v", r)
		}
	}()
	fields := strings.Fields(strings.TrimSpace(m.Text))
	if len(fields) == 0 {
		return
	}
	cmd := strings.ToLower(strings.Split(fields[0], "@")[0])
	chatID := m.Chat.ID
	switch cmd {
	case "/start", "/help":
		b.send(chatID, b.startText())
	case "/check":
		if len(fields) != 2 {
			b.send(chatID, "Формат: /check @ник или /check name@mail.com")
			return
		}
		b.handleCheck(ctx, m, fields[1])
	case "/buy":
		if len(fields) != 2 {
			b.send(chatID, "Формат: /buy <номер заказа> — номер приходит после /check")
			return
		}
		b.handleBuy(m, fields[1])
	case "/report":
		if len(fields) != 2 {
			b.send(chatID, "Формат: /report <номер заказа>")
			return
		}
		b.handleReport(m, fields[1])
	default:
		if strings.HasPrefix(cmd, "/") {
			b.send(chatID, "Не знаю такую команду. /start — что бот умеет.")
		}
	}
}

func (b *Bot) startText() string {
	return fmt.Sprintf(`Досье по нику или email — с доказательствами, а не «наугад».

/check @ник — бесплатный быстрый скан (счётчики находок).
/check name@mail.com — бесплатная проверка адреса.

Полное досье — %s USDT:
• все найденные профили со ссылками (вся база, а не короткий список)
• сайты, где используется адрес/ник
• report.html + evidence.json + journal.jsonl — каждую строку можно сверить с журналом прогона.

Оплата через Crypto Bot, досье приходит автоматически после оплаты.`, b.cfg.PriceUSDT)
}

func parseTarget(arg string) (kind, target string, err error) {
	arg = strings.TrimSpace(arg)
	if !strings.HasPrefix(arg, "@") && emailRe.MatchString(arg) {
		return "email", arg, nil
	}
	name := strings.TrimPrefix(arg, "@")
	if usernameRe.MatchString(name) {
		return "username", name, nil
	}
	return "", "", fmt.Errorf("не похоже ни на ник (3–32 латинских буквы/цифры/_), ни на email")
}

func (b *Bot) handleCheck(ctx context.Context, m *TGMessage, arg string) {
	chatID := m.Chat.ID
	userID := m.From.ID
	kind, target, err := parseTarget(arg)
	if err != nil {
		b.send(chatID, err.Error())
		return
	}
	if !b.setActive(userID, true) {
		b.send(chatID, "У тебя уже идёт скан — дождись конца, потом следующий.")
		return
	}
	defer b.setActive(userID, false)

	if wait, ok := b.store.FreeAllowed(userID, time.Duration(b.cfg.FreeCooldownSeconds)*time.Second); !ok {
		b.send(chatID, fmt.Sprintf("Бесплатный скан — раз в %d мин. Осталось подождать %s.",
			b.cfg.FreeCooldownSeconds/60, wait.Round(time.Second)))
		return
	}
	b.store.MarkFree(userID)

	id, err := NewOrderID()
	if err != nil {
		b.send(chatID, "Внутренняя ошибка, попробуй ещё раз.")
		return
	}
	order := &Order{ID: id, UserID: userID, ChatID: chatID, Kind: kind, Target: target, CreatedAt: time.Now().UTC()}

	pipeline := "username_quick.yaml"
	inputKey := "username"
	if kind == "email" {
		pipeline = "email_quick.yaml"
		inputKey = "target"
	}
	b.send(chatID, fmt.Sprintf("Сканирую %s — обычно до минуты, иногда дольше.", arg))
	runDir, err := RunWithTimeout(b.runner, ctx, time.Duration(b.cfg.QuickTimeoutSeconds)*time.Second, id, pipeline, inputKey, target)
	if err != nil {
		order.Status = StatusFailed
		order.Error = err.Error()
		_ = b.store.Save(order)
		log.Printf("quick %s: %v", id, err)
		b.send(chatID, "Скан не получился на стороне источников. Попробуй позже — бесплатный скан не сгорел.")
		return
	}
	dossier, err := ReadDossier(runDir, kind, target)
	if err != nil {
		order.Status = StatusFailed
		order.Error = err.Error()
		_ = b.store.Save(order)
		log.Printf("quick read %s: %v", id, err)
		b.send(chatID, "Скан отработал, но отчёт не собрался. Попробуй позже.")
		return
	}
	order.Status = StatusScanned
	order.RunDir = runDir
	order.QuickSummary = dossier.QuickSummary()
	if err := b.store.Save(order); err != nil {
		log.Printf("save %s: %v", id, err)
	}
	b.send(chatID, fmt.Sprintf(`%s

Полное досье — %s USDT: все ссылки и детали, вся база сайтов, HTML-отчёт с доказательствами.
Заказ %s. Оплатить: /buy %s`, order.QuickSummary, b.cfg.PriceUSDT, id, id))
}

func (b *Bot) handleBuy(m *TGMessage, id string) {
	chatID := m.Chat.ID
	order, ok := b.store.Get(id)
	if !ok || order.UserID != m.From.ID {
		b.send(chatID, "Заказ не найден. Сначала /check @ник — бот выдаст номер.")
		return
	}
	switch order.Status {
	case StatusDelivered:
		b.send(chatID, "Этот заказ уже выдан. Прислать файлы ещё раз: /report "+id)
		return
	case StatusAwaitingPayment:
		b.send(chatID, "Счёт уже выставлен:\n"+order.PayURL)
		return
	case StatusPaid, StatusRunning:
		b.send(chatID, "Оплата есть, досье собирается — придёт автоматически.")
		return
	}
	if order.Status != StatusScanned && order.Status != StatusExpired {
		b.send(chatID, "По этому заказу счёт выставить нельзя. Пройди /check заново.")
		return
	}
	if b.cb == nil {
		b.send(chatID, "Платежи пока не настроены у оператора. Попробуй позже.")
		return
	}
	inv, err := b.cb.CreateInvoice(b.cfg.PriceUSDT, "Досье: "+order.Target, order.ID)
	if err != nil {
		log.Printf("createInvoice %s: %v", id, err)
		b.send(chatID, "Не удалось выставить счёт. Попробуй ещё раз через минуту.")
		return
	}
	order.Status = StatusAwaitingPayment
	order.InvoiceID = inv.ID
	order.PayURL = inv.PayURL
	order.Amount = b.cfg.PriceUSDT
	if err := b.store.Save(order); err != nil {
		log.Printf("save invoice %s: %v", id, err)
	}
	b.send(chatID, fmt.Sprintf("Счёт на %s USDT (действует 30 минут):\n%s\n\nПосле оплаты полное досье придёт автоматически.", b.cfg.PriceUSDT, inv.PayURL))
}

func (b *Bot) pollInvoices(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, order := range b.store.AwaitingPayment() {
			status, err := b.cb.InvoiceStatus(order.InvoiceID)
			if err != nil {
				log.Printf("invoice %s: %v", order.ID, err)
				continue
			}
			switch status {
			case "paid":
				order.Status = StatusPaid
				if err := b.store.Save(order); err != nil {
					log.Printf("save paid %s: %v", order.ID, err)
					continue
				}
				go b.fulfill(ctx, order)
			case "expired":
				order.Status = StatusExpired
				_ = b.store.Save(order)
				b.send(order.ChatID, "Счёт истёк. Новый счёт: /buy "+order.ID)
			}
		}
	}
}

func (b *Bot) fulfill(ctx context.Context, order *Order) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic в fulfill %s: %v", order.ID, r)
			b.fail(order, fmt.Sprintf("panic: %v", r))
		}
	}()
	order.Status = StatusRunning
	_ = b.store.Save(order)
	b.send(order.ChatID, "Оплата получена. Собираю полное досье — обычно 3–8 минут.")
	b.admin(fmt.Sprintf("оплата: заказ %s, %s %s, %s USDT", order.ID, order.Kind, order.Target, order.Amount))

	pipeline := "username_full.yaml"
	inputKey := "username"
	if order.Kind == "email" {
		pipeline = "email_full.yaml"
		inputKey = "target"
	}
	runDir, err := RunWithTimeout(b.runner, ctx, time.Duration(b.cfg.FullTimeoutSeconds)*time.Second, order.ID, pipeline, inputKey, order.Target)
	if err != nil {
		b.fail(order, err.Error())
		return
	}
	dossier, err := ReadDossier(runDir, order.Kind, order.Target)
	if err != nil {
		b.fail(order, err.Error())
		return
	}
	outDir := filepath.Join(b.cfg.DataDir, "orders", order.ID)
	files, err := WriteReport(dossier, runDir, outDir)
	if err != nil {
		b.fail(order, err.Error())
		return
	}
	b.send(order.ChatID, dossier.FullSummary())
	for _, f := range files {
		if err := b.tg.SendDocument(order.ChatID, f, ""); err != nil {
			b.fail(order, "доставка файла: "+err.Error())
			return
		}
	}
	order.Status = StatusDelivered
	order.RunDir = runDir
	if err := b.store.Save(order); err != nil {
		log.Printf("save delivered %s: %v", order.ID, err)
	}
	b.admin(fmt.Sprintf("выдано: заказ %s (%s)", order.ID, order.Target))
}

// fail — платный заказ не доехал. Пользователю честно говорим о проблеме,
// админу — с текстом ошибки: возврат или повторная выдача остаётся на нём,
// автоматического рефанда в v1 нет, и молчать о сбое нельзя.
func (b *Bot) fail(order *Order, cause string) {
	order.Status = StatusFailed
	order.Error = cause
	_ = b.store.Save(order)
	log.Printf("заказ %s failed: %s", order.ID, cause)
	b.send(order.ChatID, "Досье не получилось собрать. Оператор уведомлён: оплату вернут или заказ переделают. Номер заказа: "+order.ID)
	b.admin(fmt.Sprintf("ПРОБЛЕМА с оплаченным заказом %s (%s %s): %s", order.ID, order.Kind, order.Target, cause))
}

func (b *Bot) handleReport(m *TGMessage, id string) {
	order, ok := b.store.Get(id)
	if !ok || order.UserID != m.From.ID {
		b.send(m.Chat.ID, "Заказ не найден.")
		return
	}
	if order.Status != StatusDelivered {
		b.send(m.Chat.ID, "Досье по этому заказу ещё не выдано.")
		return
	}
	for _, name := range []string{"report.html", "evidence.json", "journal.jsonl"} {
		path := filepath.Join(b.cfg.DataDir, "orders", id, name)
		if err := b.tg.SendDocument(m.Chat.ID, path, ""); err != nil {
			b.send(m.Chat.ID, "Не удалось отправить файл: "+err.Error())
			return
		}
	}
}

func (b *Bot) setActive(userID int64, on bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on && b.active[userID] {
		return false
	}
	if on {
		b.active[userID] = true
	} else {
		delete(b.active, userID)
	}
	return true
}

func (b *Bot) send(chatID int64, text string) {
	if err := b.tg.SendMessage(chatID, text); err != nil {
		log.Printf("sendMessage %d: %v", chatID, err)
	}
}

func (b *Bot) admin(text string) {
	if b.cfg.AdminTGID != 0 {
		b.send(b.cfg.AdminTGID, text)
	}
}
