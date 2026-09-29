package disttest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"configdist/internal/client"
	"configdist/internal/memhttp"
	"configdist/internal/protocol"
	"configdist/internal/server"
)

const serverHost = "server"

type harness struct {
	transport *memhttp.Transport
	server    *server.Server
	serverURL string
	dataPath  string
}

func setup(t *testing.T) *harness {
	t.Helper()
	tr := memhttp.New()
	dataPath := filepath.Join(t.TempDir(), "server.json")
	s, err := server.New(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	s.SetClientTransport(tr)
	tr.Bind(serverHost, s.Handler())
	t.Cleanup(func() { _ = s.Close() })
	return &harness{transport: tr, server: s, serverURL: "http://" + serverHost, dataPath: dataPath}
}

// restartServer 模拟服务端重启：状态从同一磁盘文件恢复。
func (h *harness) restartServer(t *testing.T) {
	t.Helper()
	h.server.Close()
	s, err := server.New(h.dataPath)
	if err != nil {
		t.Fatal(err)
	}
	s.SetClientTransport(h.transport)
	h.transport.Bind(serverHost, s.Handler())
	h.server = s
	t.Cleanup(func() { _ = s.Close() })
}

func (h *harness) startClient(t *testing.T, id string) *client.Client {
	t.Helper()
	return h.startClientIn(t, id, filepath.Join(t.TempDir(), id))
}

func (h *harness) startClientIn(t *testing.T, id, dir string) *client.Client {
	t.Helper()
	c, err := client.New(id, h.serverURL, dir)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(h.transport)
	h.transport.Bind(id, c.Handler())
	// 内存模式只启动协调循环；id 同时作为服务端回连的主机名。
	c.Start(id)
	t.Cleanup(c.Close)
	time.Sleep(20 * time.Millisecond)
	return c
}

// stopClient 模拟客户端短暂断开：解绑处理器后协调循环仍会重试注册/同步。
func (h *harness) stopClient(c *client.Client) {
	h.transport.Unbind(c.ID())
	c.Close()
}

func (h *harness) publish(t *testing.T, id string, seq int, content any) {
	t.Helper()
	body, _ := json.Marshal(protocol.Config{ID: id, Sequence: seq, Content: content})
	resp := h.mustDo(t, http.MethodPost, h.serverURL+"/publish", body)
	resp.Body.Close()
}

func (h *harness) report(t *testing.T, r protocol.ReportRequest) {
	t.Helper()
	body, _ := json.Marshal(r)
	resp := h.mustDo(t, http.MethodPost, h.serverURL+"/report", body)
	resp.Body.Close()
}

func (h *harness) status(t *testing.T) protocol.StatusResponse {
	t.Helper()
	resp := h.mustDo(t, http.MethodGet, h.serverURL+"/status", nil)
	defer resp.Body.Close()
	var st protocol.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (h *harness) reconcile(t *testing.T) []map[string]any {
	t.Helper()
	resp := h.mustDo(t, http.MethodPost, h.serverURL+"/reconcile", nil)
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *harness) mustDo(t *testing.T, method, url string, body []byte) *http.Response {
	t.Helper()
	var rdr io.Reader = http.NoBody
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
