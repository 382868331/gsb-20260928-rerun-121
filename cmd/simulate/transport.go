package main

import (
	"net/http"
	"net/http/httptest"
)

// inProcessTransport 不经过任何真实端口，直接把请求交给 http.Handler，
// 用于在同一进程内连接多个“本地虚拟客户端”。
type inProcessTransport struct{ handler http.Handler }

func (t *inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newInProcessClient(h http.Handler) *http.Client {
	return &http.Client{Transport: &inProcessTransport{handler: h}}
}
