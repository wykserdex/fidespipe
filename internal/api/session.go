package api

// H4: сессия человека.
//
// Модель входа (было: `?k=<128-битный секрет>` в ссылке из терминала):
//
//	1. `fidespipe gui` печатает ОДНОРАЗОВЫЙ КОД (12 символов, окно 10 минут) и ссылку
//	   без секрета. Сам секрет сессии в терминале не появляется.
//	2. Человек открывает GUI одним из двух способов:
//	     • `fidespipe gui --open` / fidespipegui / гейт в `fidespipe mcp` открывают окно с
//	       `?c=<код>` — сервер обменивает код на cookie и редиректит на тот же
//	       адрес без кода;
//	     • человек открывает адрес сам и вводит код в поле на странице
//	       (POST /api/session).
//	3. Cookie — не секрет: это отдельный токен с TTL (12 ч) в памяти сервера.
//	   Код после обмена мёртв, а не «секрет, утекавший в логи».
//
// Что это защищает (честно, как и раньше): агент/процесс, который умеет только
// слать HTTP на локальный порт, не может ни прочитать журналы и входы шагов, ни
// запустить ран, ни одобрить гейт — cookie в его рук не попадает. От процесса
// того же пользователя ОС, который читает терминал, память или профиль
// браузера, это не защищает.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wykserdex/fidespipe/internal/gate"
)

const (
	// sessionCookiePrefix — префикс имени cookie. Порт дописывается:
	// cookie в браузере портом не различаются, и без порта в имени два
	// экземпляра на localhost затирали сессию друг друга.
	sessionCookiePrefix = "fidespipe_session"

	// pairingCodeLen — длина одноразового кода (12 символов base32 = 60 бит).
	pairingCodeLen = 12
	// pairingCodeTTL — окно жизни кода. Длиннее не нужно: код либо открыт
	// браузер сразу, либо человек вводит его руками.
	pairingCodeTTL = 10 * time.Minute
	// defaultSessionTTL — время жизни cookie сессии. Раньше cookie жла бессрочно
	// и была равна секрету, то есть утечка была навсегда.
	defaultSessionTTL = 12 * time.Hour
	// maxSessions — сколько живых сессий держим на сервер (браузер + WebView2 +
	// вторая вкладка). Больше — вытесняем самую старую.
	maxSessions = 8
	// maxPairingFailures / pairingLockout — код короткий, но перебирать его
	// локальным процессом нельзя: после нескольких неудач вход закрывается.
	maxPairingFailures = 5
	pairingLockout     = time.Minute
)

// base32 без неоднозначных символов (0/O, 1/I/L) — код читают и перепечатывают.
const pairingAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

var (
	errPairingBad      = errors.New("неверный код входа")
	errPairingDead     = errors.New("код входа уже использован или истёк")
	errPairingLocked   = errors.New("слишком много неудачных попыток входа")
	errPairingNoRandom = errors.New("система не дала энтропию для токена сессии")
)

// NewPairingCode — одноразовый код обмена: 12 символов из crypto/rand (60 бит),
// группами по 4. Печатается в терминал и обменивается на cookie сессии.
func NewPairingCode() (string, error) {
	out := make([]byte, pairingCodeLen)
	buf := make([]byte, pairingCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		out[i] = pairingAlphabet[int(b)%len(pairingAlphabet)]
	}
	return string(out[0:4]) + "-" + string(out[4:8]) + "-" + string(out[8:12]), nil
}

// normalizePairingCode — приводит введённый человеком код к каноническому виду
// (заглавные, без дефисов и пробелов): перепечатывают код руками, и «ABCD-EFGH-IJK»
// должно совпасть с тем, что напечатано.
func normalizePairingCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "", " ", "").Replace(code)
}

// Server session state (в памяти процесса; перезапуск сервера обнуляет сессии).

type sessionEntry struct {
	token   string
	created time.Time
	expires time.Time
}

// EnableSession — включить требование сессии человека. Аргумент: ОДНОРАЗОВЫЙ
// код обмена (NewPairingCode), а не постоянный секрет. Пустая строка — сессия
// выключена (--no-session, встраивание, тесты): тогда открыт весь /api/*.
func (s *Server) EnableSession(code string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.setPairingCodeLocked(code)
	s.sessions = map[string]sessionEntry{}
}

// RotatePairingCode — выдать новый одноразовый код (код одноразовый: после
// обмена он мёртв, а человеку может понадобиться второй браузер или вторая
// вкладка). Старые сессии при этом живут — смена кода их не выкидывает.
func (s *Server) RotatePairingCode(code string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.setPairingCodeLocked(code)
}

// setPairingCodeLocked — сбросить состояние обмена кода. Живые сессии не
// трогает: их инвалидирует только EnableSession (полный перезапуск входа) или
// истечение TTL. Разделение намеренное — раньше Rotate вызывал EnableSession
// и вопреки комментарию выкидывал все сессии, включая ту, с которой человек
// ждал гейт.
func (s *Server) setPairingCodeLocked(code string) {
	s.PairingCode = normalizePairingCode(code)
	s.pairingUsed = false
	s.pairingExpires = time.Now().Add(pairingCodeTTL)
	s.pairingFailures = 0
	s.pairingBlockedUntil = time.Time{}
}

// sessionRequired — сессия включена? Пустой PairingCode = режим без защиты.
func (s *Server) sessionRequired() bool { return s.PairingCode != "" }

// sessionHash — что пишется в журнал вместо cookie (gate_decision.session):
// короткий хэш ТОКЕНА сессии, не кода и не секрета.
func (s *Server) sessionHash(r *http.Request) string {
	c, err := r.Cookie(s.policy.cookieName())
	if err != nil || c.Value == "" {
		return ""
	}
	h := sha256.Sum256([]byte("fidespipe-session:" + c.Value))
	return hex.EncodeToString(h[:6])
}

// sessionValid — токен есть и не истёк.
func (s *Server) sessionValid(token string) bool {
	if token == "" {
		return false
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	e, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(e.expires) {
		delete(s.sessions, token)
		return false
	}
	return subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) == 1
}

// hasSession — сессия выключена (тесты, --no-session) или cookie верный.
func (s *Server) hasSession(r *http.Request) bool {
	if !s.sessionRequired() {
		return true
	}
	c, err := r.Cookie(s.policy.cookieName())
	if err != nil {
		return false
	}
	return s.sessionValid(c.Value)
}

// requireSession — 401 без cookie. true — можно продолжать.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if s.hasSession(r) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(401)
	w.Write([]byte(`{"error":"нужна сессия человека: откройте адрес GUI и введите одноразовый код из терминала fidespipe","code":"E_SESSION_REQUIRED"}` + "\n"))
	return false
}

// exchangePairingCode — код → новый токен сессии. Код одноразовый: первый
// успешный обмен его убивает.
func (s *Server) exchangePairingCode(code string) (string, error) {
	now := time.Now()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if now.Before(s.pairingBlockedUntil) {
		return "", errPairingLocked
	}
	if s.pairingUsed || now.After(s.pairingExpires) {
		return "", errPairingDead
	}
	if subtle.ConstantTimeCompare([]byte(normalizePairingCode(code)), []byte(s.PairingCode)) != 1 {
		s.pairingFailures++
		if s.pairingFailures >= maxPairingFailures {
			s.pairingFailures = 0
			s.pairingBlockedUntil = now.Add(pairingLockout)
		}
		return "", errPairingBad
	}
	s.pairingUsed = true
	token := s.mintSessionLocked(now)
	if token == "" {
		// энтропии нет — вход не выдаём и код НЕ тратим: иначе человек
		// остался бы без входа до перезапуска (fail-closed)
		s.pairingUsed = false
		return "", errPairingNoRandom
	}
	return token, nil
}

// mintSessionLocked — выпустить токен с TTL, вытеснив самый старый при нехватке
// места. Секрет/код в токен не входит: утечка cookie не даёт войти снова.
func (s *Server) mintSessionLocked(now time.Time) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand не сработал — вход не выдаём (fail-closed).
		return ""
	}
	if s.sessions == nil {
		s.sessions = map[string]sessionEntry{}
	}
	for token, e := range s.sessions {
		if now.After(e.expires) {
			delete(s.sessions, token)
		}
	}
	for len(s.sessions) >= maxSessions {
		oldestToken, oldest := "", time.Time{}
		for token, e := range s.sessions {
			if oldest.IsZero() || e.created.Before(oldest) {
				oldestToken, oldest = token, e.created
			}
		}
		delete(s.sessions, oldestToken)
	}
	token := hex.EncodeToString(b[:])
	s.sessions[token] = sessionEntry{token: token, created: now, expires: now.Add(s.policy.ttl)}
	return token
}

// setSessionCookie — cookie сессии. Secure решает конфигурация (внешняя схема),
// а не заголовок запроса; MaxAge/Expires дают TTL, которого раньше не было.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.policy.cookieName(), Value: token, Path: "/",
		HttpOnly: true, Secure: s.policy.SecureCookie(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.policy.ttl / time.Second),
		Expires:  time.Now().Add(s.policy.ttl),
	})
}

// ── /api/session: обмен кода на cookie ───────────────────────────────────

// handleSession — GET/POST /api/session.
//
// Открыт без cookie — единственная точка входа, где это обязано быть: без неё
// человек не смог бы вообще получить cookie. Открытость не стоит ничего, пока
// обмен требует одноразовый код из терминала, которого у чужого процесса нет.
//
//	GET  → {"authenticated":bool,"required":bool,...} — «есть ли уже сессия»
//	POST {"code":"XXXX-XXXX-XXXX"} → cookie сессии; 401 на неверном/использованном коде
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		authed := s.hasSession(r)
		out := map[string]interface{}{
			"authenticated": authed,
			"required":      s.sessionRequired(),
		}
		if authed {
			out["expires_in"] = int(s.policy.ttl / time.Second)
		} else {
			out["how"] = "одноразовый код из терминала fidespipe → POST /api/session {\"code\":\"XXXX-XXXX-XXXX\"}"
		}
		writeJSON(w, 200, out)
	case "POST":
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "json: "+err.Error(), 400)
			return
		}
		token, err := s.exchangePairingCode(req.Code)
		if err != nil {
			if errors.Is(err, errPairingLocked) {
				writeJSON(w, 429, map[string]interface{}{
					"error": "слишком много неудачных попыток; подождите минуту", "code": "E_SESSION_CODE_INVALID",
				})
				return
			}
			writeJSON(w, 401, map[string]interface{}{
				"error": "код входа не подошёл: " + err.Error(), "code": "E_SESSION_CODE_INVALID",
			})
			return
		}
		s.setSessionCookie(w, r, token)
		writeJSON(w, 200, map[string]interface{}{
			"authenticated": true,
			"expires_in":    int(s.policy.ttl / time.Second),
		})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// sessionHandshake — GET ...?c=<одноразовый код>: обмен кода на cookie и
// редирект на тот же адрес без кода (код не остаётся в адресной строке).
// true — ответ уже записан.
func (s *Server) sessionHandshake(w http.ResponseWriter, r *http.Request) bool {
	if !s.sessionRequired() || (r.Method != "GET" && r.Method != "HEAD") {
		return false
	}
	k := r.URL.Query().Get("c")
	if k == "" {
		return false
	}
	token, err := s.exchangePairingCode(k)
	if err != nil {
		http.Error(w, "код входа не подошёл: "+err.Error(), http.StatusUnauthorized)
		return true
	}
	s.setSessionCookie(w, r, token)
	q := r.URL.Query()
	q.Del("c")
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
	return true
}

// ── v0.9: встраивание (fidespipe mcp) ────────────────────────────────────────
// MCP-сервер исполняет раны сам, а решения гейтов и отмена приходят от
// человека через этот HTTP-сервер (с сессией). Регистрация внешнего рана
// делает его гейт/отмену доступными по /api/runs/<id>/gate|cancel.

// AttachRun — зарегистрировать гейт и/или отмену внешнего рана.
func (s *Server) AttachRun(id string, ui *gate.ChannelUI, cancel context.CancelFunc) {
	if ui != nil {
		s.setGate(id, ui)
	}
	if cancel != nil {
		s.setCancel(id, cancel)
	}
}

// DetachRun — ран завершён: гейт закрыт, отмена снята.
func (s *Server) DetachRun(id string) {
	s.clearGate(id)
	s.clearCancel(id)
}
