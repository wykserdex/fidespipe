package plugin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// Egress-фильтр для плагинов, которым объявлен any_host.
//
// Зачем он нужен при явном any_host: пользователь сознательно разрешил сеть, но
// «весь интернет» — это не «можно читать почту и слать в API». Плагин по
// определению читает vault, и без фильтра он может унести прочитанное
// любому хосту. Фильтр превращает любое разрешение в список назначений.
//
// Модель: прокси живёт в НЕизолированном процессе Fidespipe, слушает loopback, а
// песочница может достать только до него. Внутри прокси TLS end-to-end: мы не
// расшифровываем, только проверяем назначение CONNECT и ретранслируем байты.
//
// Перечисленные здесь инварианты неочевидны и обязаны остаться:
//   - резолвятся все A/AAAA, проверяется КАЖДЫЙ адрес, и соединение идёт по
//     литеральному IP: иначе между проверкой и dial DNS успевает подмениться;
//   - приватные и link-local адреса запрещены всегда, включая 169.254.169.254 —
//     иначе плагин читает облачные метаданные и внутренние сервисы хоста;
//   - CONNECT разрешён только на TLS-порты, иначе прокси становится туннелем в
//     любой cleartext-протокол;
//   - http.Server.Shutdown не трогает hijacked-соединения, поэтому их надо
//     закрывать самому, иначе зависший апстрим не даст завершиться.

// denyNetwork — адреса, куда ходить нельзя никогда.
func denyNetwork(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	// CGNAT 100.64.0.0/10 — не private по IsPrivate, но для нас тоже локальный.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xC0 == 64 {
			return true
		}
		return false
	}
	// IPv6 unique-local fc00::/7.
	if len(ip) == net.IPv6len && ip[0]&0xFE == 0xFC {
		return true
	}
	return false
}

// hostAllowed — совпадение хоста и порта с объявлением.
func hostAllowed(allow []pipeline.NetworkPermission, host string, port int) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, np := range allow {
		if np.Port > 0 && np.Port != port {
			continue
		}
		if np.AnyHost {
			return true
		}
		if matchHost(strings.ToLower(np.Host), h) {
			return true
		}
	}
	return false
}

// matchHost — точное совпадение либо шаблон с ведущей точкой или "*.".
func matchHost(pattern, host string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return false
}

// egress — запущенный фильтр на loopback.
type egress struct {
	ln       net.Listener
	srv      *http.Server
	allow    []pipeline.NetworkPermission
	tlsPorts map[int]bool

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func newEgress(allow []pipeline.NetworkPermission) (*egress, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress listen: %w", err)
	}
	e := &egress{
		ln:       ln,
		allow:    allow,
		tlsPorts: map[int]bool{443: true, 8443: true},
		conns:    map[net.Conn]struct{}{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", e.handle)
	e.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

// addr — что отдать плагину в HTTP_PROXY/HTTPS_PROXY.
func (e *egress) addr() string { return "http://" + e.ln.Addr().String() }

func (e *egress) env() []string {
	a := e.addr()
	return []string{
		"HTTP_PROXY=" + a, "http_proxy=" + a,
		"HTTPS_PROXY=" + a, "https_proxy=" + a,
	}
}

// stop закрывает сервер и все живые туннели.
func (e *egress) stop() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	conns := make([]net.Conn, 0, len(e.conns))
	for c := range e.conns {
		conns = append(conns, c)
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = e.srv.Shutdown(ctx)
	_ = e.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (e *egress) track(c net.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		_ = c.Close()
		return
	}
	e.conns[c] = struct{}{}
}

func (e *egress) untrack(c net.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.conns, c)
}

// resolveChecked резолвит и проверяет все адреса. Возвращает адрес для dial.
func (e *egress) resolveChecked(host string) (string, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("нет A/AAAA для %s", host)
	}
	for _, ip := range ips {
		if denyNetwork(ip) {
			return "", fmt.Errorf("адрес %s для %s попадает в запрещённый диапазон", ip, host)
		}
	}
	// Соединяемся по литеральному IP: между проверкой и dial DNS может
	// вернуть другой адрес (DNS rebinding).
	return ips[0].String(), nil
}

func splitHostPort(hostport string) (string, int) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, 0
	}
	port := 0
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		port = 0
	}
	return h, port
}

func (e *egress) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		e.handleConnect(w, r)
		return
	}
	e.handleForward(w, r)
}

func (e *egress) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port := splitHostPort(r.Host)
	if port == 0 {
		port = 443
	}
	if !e.tlsPorts[port] {
		http.Error(w, "egress: CONNECT разрешён только на TLS-порты", http.StatusForbidden)
		return
	}
	if !hostAllowed(e.allow, host, port) {
		http.Error(w, "egress: хост не объявлен в permissions.network", http.StatusForbidden)
		return
	}
	ip, err := e.resolveChecked(host)
	if err != nil {
		http.Error(w, "egress: "+err.Error(), http.StatusForbidden)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress: hijack недоступен", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	e.track(client)
	defer e.untrack(client)

	up, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(port)), 10*time.Second)
	if err != nil {
		_, _ = client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		_ = client.Close()
		return
	}
	e.track(up)
	defer e.untrack(up)

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = up.Close()
		return
	}
	// TLS end-to-end: дальше только байты, мы их не читаем.
	relay(client, up)
}

func (e *egress) handleForward(w http.ResponseWriter, r *http.Request) {
	host, port := splitHostPort(r.Host)
	if port == 0 {
		port = 80
	}
	if !hostAllowed(e.allow, host, port) {
		http.Error(w, "egress: хост не объявлен в permissions.network", http.StatusForbidden)
		return
	}
	ip, err := e.resolveChecked(host)
	if err != nil {
		http.Error(w, "egress: "+err.Error(), http.StatusForbidden)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, fmt.Sprint(port))}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			outReq.Header.Add(k, v)
		}
	}
	// Host-заголовок должен указывать на исходное имя, а не на IP.
	outReq.Host = r.Host
	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// relay — двусторонняя перекачка с дедлайном, чтобы зависший апстрим не держал
// соединение вечно.
func relay(a, b net.Conn) {
	deadline := time.Now().Add(5 * time.Minute)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.SetDeadline(deadline)
	_ = b.SetDeadline(deadline)
}
