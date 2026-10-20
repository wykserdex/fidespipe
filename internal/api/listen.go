package api

// H4: чем сервер объявляет себя наружу и чему верит.
//
// Трёх вещей в коде раньше не было, и без них утверждение «сервер слушает
// только локально» было неправдой:
//
//  1. allow-list Host. Без него страница с чужого домена, чей DNS указывает на
//     127.0.0.1 (DNS-rebinding), читает журналы и входы/выходы шагов: браузер
//     честно шлёт `Host: чужой.домен`, а сервер такой Host принимал.
//  2. Понятия «доверенный прокси» не было: X-Forwarded-Host/Proto приходили от
//     любого клиента. Ими можно было назвать любой Origin своим и поставить
//     Secure на cookie. Теперь эти заголовки читаются только при явном
//     --trusted-proxy, и только для флага Secure у cookie.
//  3. Origin сравнивался со значением из того же запроса (csrfRequestHost брал
//     r.Host, а Origin сравнивался с ним) — то есть проверка сравнивала запрос
//     с самим собой. Теперь Origin сверяется с тем, что сервер объявляет САМ:
//     allow-list Host + внешняя схема. Значения из запроса в ожидаемую
//     сторону не подставляются.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultPortHTTP  = "80"
	defaultPortHTTPS = "443"
)

// ListenOptions — ровно то, что задал человек флагами `fidespipe gui`.
type ListenOptions struct {
	// Addr — host:port для прослушивания ("127.0.0.1:8765"; порт 0 — любой свободный).
	Addr string
	// ResolvedPort — порт ПОСЛЕ net.Listen, когда в Addr стоял 0. Имя cookie и
	// allow-list должны знать настоящий порт, а не 0. Пусто — берём порт из Addr.
	ResolvedPort string
	// AllowRemote — --allow-remote: разрешить не-loopback адрес.
	AllowRemote bool
	// NoSession — --no-session: сессия человека выключена (осознанный отказ от защиты).
	NoSession bool
	// PublicHosts — --public-host host[:port] (повторяемый): Host'ы и Origin'ы,
	// которым мы верим. Смысл есть только вместе с --allow-remote.
	PublicHosts []string
	// TrustedProxy — --trusted-proxy: за нами есть прокси. Единственное место,
	// где читается X-Forwarded-Proto (для Secure у cookie). X-Forwarded-Host не
	// читается вообще: внешнее имя задаёт оператор через --public-host.
	TrustedProxy bool
	// ExternalScheme — схема, под которой человек открывает GUI: "http" или
	// "https" (https = TLS-прокси перед нами). Пусто → "https" при TrustedProxy,
	// иначе "http".
	ExternalScheme string
	// SessionTTL — время жизни cookie сессии. Ноль → defaultSessionTTL.
	SessionTTL time.Duration
}

// ValidateListen — проверка флагов ДО поднятия сервера. Отдельная чистая
// функция: её зовёт CLI (где нужен отказ с внятным текстом) и тесты (где
// поднимать сервер незачем).
func ValidateListen(o ListenOptions) error {
	host, _, err := splitListenAddr(o.Addr)
	if err != nil {
		return err
	}
	if _, err := externalScheme(o); err != nil {
		return err
	}
	scheme, _ := externalScheme(o)
	for _, h := range o.PublicHosts {
		if normalizeHostPort(h, scheme) == "" {
			return fmt.Errorf("--public-host=%q: ожидается имя или IP, при необходимости с портом", h)
		}
	}
	if !isLoopbackHost(host) {
		if !o.AllowRemote {
			return fmt.Errorf("--listen %s: адрес не loopback, а --allow-remote не задан. "+
				"Так сервер читает журналы, входы и выходы шагов и принимает решения гейта от любого, "+
				"кто дотянулся до порта. Либо вернитесь к 127.0.0.1, либо скажите это явно: "+
				"--allow-remote --public-host=<имя или IP>", o.Addr)
		}
		if o.NoSession {
			return fmt.Errorf("--no-session вместе с не-loopback адресом запрещён: " +
				"внешний доступ без сессии человека открывает все /api/* каждому в сети")
		}
		if isWildcardHost(host) && len(o.PublicHosts) == 0 {
			return fmt.Errorf("--listen %s слушает ВСЕ интерфейсы, а --public-host не задан: "+
				"тогда почти каждый Host чужой, и сервер отвергал бы всё подряд. "+
				"Перечислите, как вас зовут снаружи: --public-host=192.168.1.10", o.Addr)
		}
	}
	if len(o.PublicHosts) > 0 && !o.AllowRemote {
		return fmt.Errorf("--public-host=%s без --allow-remote: на %s внешние подключения "+
			"и так не принимаются, а Host из allow-list только вводил бы в заблуждение",
			o.PublicHosts[0], o.Addr)
	}
	return nil
}

// ListenPolicy — вычисленная из флагов политика доверия к HTTP-запросу.
type ListenPolicy struct {
	addr    string
	port    string
	hosts   map[string]bool // нормализованные host:port, которым верим
	scheme  string          // внешняя схема: http или https
	proxied bool            // --trusted-proxy
	ttl     time.Duration   // время жизни сессии
	remote  bool            // --allow-remote
}

// NewListenPolicy — проверяет флаги и строит политику. Пустая политика (сервер
// без ConfigureListen) = loopback на любом порту, http, без прокси: этого
// хватает встраиванию и тестам, и это не оставляет открытым ни одну из
// проверенных дыр (см. hostAllowed).
func NewListenPolicy(o ListenOptions) (ListenPolicy, error) {
	if err := ValidateListen(o); err != nil {
		return ListenPolicy{}, err
	}
	scheme, _ := externalScheme(o)
	host, port, _ := splitListenAddr(o.Addr)
	if rp := strings.TrimSpace(o.ResolvedPort); rp != "" {
		if !isDigits(rp) {
			return ListenPolicy{}, fmt.Errorf("ResolvedPort=%q: порт должен быть числом", o.ResolvedPort)
		}
		port = rp
	}
	p := ListenPolicy{
		addr:    o.Addr,
		port:    port,
		hosts:   map[string]bool{},
		scheme:  scheme,
		proxied: o.TrustedProxy,
		ttl:     o.SessionTTL,
		remote:  o.AllowRemote,
	}
	if p.ttl <= 0 {
		p.ttl = defaultSessionTTL
	}
	// Явно заданный адрес — часть allow-list: слушать на 192.168.1.10 значит
	// принимать Host: 192.168.1.10:<порт>.
	if isConcreteHost(host) {
		p.hosts[net.JoinHostPort(strings.ToLower(host), port)] = true
	}
	for _, h := range o.PublicHosts {
		if v := normalizeHostPort(h, scheme); v != "" {
			p.hosts[v] = true
		}
	}
	return p, nil
}

// cookieName — имя cookie сессии. Порт в имени обязателен: cookie в браузере
// портом не различаются, поэтому два экземпляра на localhost:8765 и
// localhost:8766 затирали сессию друг друга (второй перезаписывал значение
// первого, и первый начинал отдавать 401).
func (p ListenPolicy) cookieName() string {
	if p.port == "" || p.port == "0" {
		return sessionCookiePrefix
	}
	return sessionCookiePrefix + "_" + p.port
}

// SecureCookie — Secure у cookie сессии.
//
// Решение принимает КОНФИГУРАЦИЯ, а не заголовок запроса. Раньше флаг ставился
// по X-Forwarded-Proto от любого клиента: значение, которое клиент сам же и
// прислал, решало, станет ли cookie защищённой. Теперь X-Forwarded-Proto читается
// только при явном --trusted-proxy (оператор сказал, что за ним прокси), и даже
// тогда Secure уже включён схемой из конфигурации.
func (p ListenPolicy) SecureCookie(r *http.Request) bool {
	if p.scheme == "https" {
		return true
	}
	if !p.proxied {
		return false
	}
	return strings.EqualFold(forwardedFirst(r.Header.Get("X-Forwarded-Proto")), "https")
}

// hostAllowed — Host запроса в allow-list (DNS-rebinding).
//
// Loopback-трио разрешено всегда и на любом порту. Это НЕ ослабление защиты
// от rebinding: браузер страницы с чужого домена шлёт `Host: чужой.домен`
// (порт в нём — порт её собственного URL), а не 127.0.0.1. Что действительно
// важно — имя хоста, и именно оно проверяется. Плюс так не ломается встраивание
// и тесты, где порт известен только после Listen.
func (p ListenPolicy) hostAllowed(raw string) bool {
	h := normalizeHostPort(raw, p.scheme)
	if h == "" {
		return false
	}
	if p.hosts[h] {
		return true
	}
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		host = h
	}
	return isLoopbackHost(host)
}

// originAllowed — Origin в списке origin'ов, которые сервер объявляет сам.
//
// Ключевое отличие от прежней проверки: ожидаемая сторона берётся из политики
// (allow-list Host + внешняя схема), а не из полей того же запроса. Схема
// Origin'а и его имя больше не могут быть заданы тем же запросом, который
// проверяется.
func (p ListenPolicy) originAllowed(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, p.scheme) {
		return false
	}
	if strings.ContainsAny(u.Host, " \t\r\n") {
		return false
	}
	return p.hostAllowed(u.Host)
}

// LinkScheme — схема в адресе, который печатается человеку. Отдельная функция,
// а не поле: вызывающий (CLI) обязан печатать ровно ту схему, которую сервер
// потом примет в Origin, иначе адрес на экране и проверка разойдутся.
func LinkScheme(o ListenOptions) string {
	s, err := externalScheme(o)
	if err != nil {
		return "http"
	}
	return s
}

func externalScheme(o ListenOptions) (string, error) {
	s := strings.ToLower(strings.TrimSpace(o.ExternalScheme))
	switch s {
	case "":
		if o.TrustedProxy {
			return "https", nil
		}
		return "http", nil
	case "http", "https":
		return s, nil
	default:
		return "", fmt.Errorf("--origin-scheme=%q: ожидается http или https", o.ExternalScheme)
	}
}

// splitListenAddr — host:port из --listen. Пустой host (":8765") = все
// интерфейсы. Порт 0 разрешён: адрес узнаём после Listen.
func splitListenAddr(addr string) (host, port string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", fmt.Errorf("--listen: пусто")
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		// адрес без порта: ":8765" SplitHostPort разбирает, а вот "127.0.0.1" —
		// ошибка формата, и это нельзя молча чинить портом по умолчанию.
		return "", "", fmt.Errorf("--listen=%q: нужен host:port (например 127.0.0.1:8765)", addr)
	}
	n := 0
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n < 0 || n > 65535 {
		return "", "", fmt.Errorf("--listen=%q: порт должен быть числом 0..65535", addr)
	}
	return h, p, nil
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isWildcardHost — адрес, на котором сервер принимает подключения от всех интерфейсов.
func isWildcardHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if h == "" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsUnspecified()
	}
	return false
}

// isConcreteHost — адрес, который можно предъявить в Host (не 0.0.0.0/::).
func isConcreteHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return !ip.IsUnspecified()
	}
	return true
}

// normalizeHostPort — "host:port" с портом ВСЕГДА. Браузер опускает порт, если
// он дефолтный для схемы, и без нормализации один и тот же адрес давал бы два
// разных ключа allow-list.
func normalizeHostPort(raw, scheme string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "/\\?#@ \t\r\n") {
		return ""
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host = strings.Trim(raw, "[]")
		port = ""
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if port == "" {
		port = portForScheme(scheme)
	}
	if !isDigits(port) {
		return ""
	}
	return net.JoinHostPort(host, port)
}

func portForScheme(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return defaultPortHTTPS
	}
	return defaultPortHTTP
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func forwardedFirst(v string) string {
	return strings.TrimSpace(strings.Split(v, ",")[0])
}
