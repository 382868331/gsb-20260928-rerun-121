// Package memhttp 提供按主机名路由的内存 HTTP 传输，
// 让服务端与多个本地虚拟客户端在不开任何真实端口的情况下通信。
package memhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

type Transport struct {
	mu       sync.RWMutex
	handlers map[string]http.Handler
}

func New() *Transport {
	return &Transport{handlers: map[string]http.Handler{}}
}

// Host 把逻辑地址（如 client-1）转成该传输使用的 URL host。
func Host(name string) string { return name }

func (t *Transport) Bind(name string, h http.Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers[name] = h
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	h, ok := t.handlers[req.URL.Host]
	if !ok {
		return nil, fmt.Errorf("memhttp: 未注册的主机 %q", req.URL.Host)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), nil
}
