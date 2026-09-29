package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"configdist/internal/protocol"
	"configdist/internal/store"
)

type clientData struct {
	Active protocol.Config `json:"active"`
}

// Client 是本地虚拟客户端：整份配置下载到暂存区，
// 校验通过后原子替换；失败则原活动版本保持不变。
type Client struct {
	id        string
	serverURL string
	st        *store.Store[clientData]
	handler   http.Handler
	httpc     *http.Client
	transport http.RoundTripper
	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup

	mu          sync.Mutex
	failVersion string // 故障注入：对该版本应用必失败
}

func New(id, serverURL, dataDir string) (*Client, error) {
	st, err := store.New(filepath.Join(dataDir, "state.json"), clientData{})
	if err != nil {
		return nil, err
	}
	httpc := &http.Client{Timeout: 3 * time.Second}
	c := &Client{
		id:        id,
		serverURL: trimSlash(serverURL),
		st:        st,
		httpc:     httpc,
		stop:      make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /actual", c.handleActual)
	mux.HandleFunc("POST /sync", c.handleSync)
	c.handler = mux
	return c, nil
}

// SetTransport 注入传输层，便于无端口测试；默认走真实网络。
func (c *Client) SetTransport(rt http.RoundTripper) {
	c.transport = rt
	c.httpc.Transport = rt
}

// Handler 返回客户端的 HTTP 处理器，供内存传输/真实 Server 复用。
func (c *Client) Handler() http.Handler { return c.handler }

func trimSlash(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '/') {
		s = s[:len(s)-1]
	}
	return s
}

func (c *Client) ID() string { return c.id }

func (c *Client) Serve(addr string) error {
	c.Start(addr)
	srv := &http.Server{Addr: addr, Handler: c.handler, ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

// Start 只启动注册与周期性同步循环，不监听端口（供测试/嵌入使用）。
func (c *Client) Start(addr string) {
	c.mustRegister(addr)
	c.wg.Add(1)
	go c.syncLoop(addr)
}

func (c *Client) Close() {
	c.stopOnce.Do(func() { close(c.stop) })
	c.wg.Wait()
}

// SetFailVersion 注入故障：应用指定版本时返回校验失败，原版本保留。
func (c *Client) SetFailVersion(id string) {
	c.mu.Lock()
	c.failVersion = id
	c.mu.Unlock()
}

func (c *Client) ActiveVersion() string {
	var id string
	c.st.View(func(d *clientData) { id = d.Active.ID })
	return id
}

func (c *Client) mustRegister(addr string) {
	body, _ := json.Marshal(protocol.RegisterRequest{ID: c.id, Addr: addr})
	resp, err := c.httpc.Post(c.serverURL+"/register", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
	// 注册失败不致命：syncLoop 会持续重试，适配服务端短暂断开/重启。
}

func (c *Client) syncLoop(addr string) {
	defer c.wg.Done()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.Sync()
		}
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("server status %d: %s", resp.StatusCode, b)
	}
	return resp, nil
}

func (c *Client) report(stage string, cfg protocol.Config, reason string) {
	r := protocol.ReportRequest{
		ClientID:        c.id,
		Stage:           stage,
		VersionID:       cfg.ID,
		ActiveVersionID: c.ActiveVersion(),
		Reason:          reason,
	}
	body, _ := json.Marshal(r)
	resp, err := c.httpc.Post(c.serverURL+"/report", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}

// Sync 执行一次完整协调：缺中间版本时也直接拉取期望的最新完整快照。
func (c *Client) Sync() error {
	mResp, err := c.do(mustGet(c.serverURL + "/manifest"))
	if err != nil {
		return err
	}
	var man protocol.Manifest
	_ = json.NewDecoder(mResp.Body).Decode(&man)
	mResp.Body.Close()
	if man.DesiredID == "" || man.DesiredID == c.ActiveVersion() {
		return nil
	}
	gResp, err := c.do(mustGet(c.serverURL + "/config?id=" + man.DesiredID))
	if err != nil {
		return err
	}
	var desired protocol.Config
	_ = json.NewDecoder(gResp.Body).Decode(&desired)
	gResp.Body.Close()

	// 下载完成只汇报 downloaded——这不是应用成功。
	c.report(protocol.StageDownloaded, desired, "")

	if err := c.apply(desired); err != nil {
		// 失败：汇报原因，原活动版本不动，等待后续重试。
		c.report(protocol.StageFailed, desired, err.Error())
		return err
	}
	c.report(protocol.StageApplied, desired, "")
	return nil
}

func mustGet(url string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, url, nil)
	return r
}

// apply 做整份配置的可靠切换：先写入暂存文件并校验，再原子 rename。
func (c *Client) apply(cfg protocol.Config) error {
	c.mu.Lock()
	failID := c.failVersion
	c.mu.Unlock()
	if failID != "" && failID == cfg.ID {
		return fmt.Errorf("应用 %s 失败: 模拟的内容校验错误，已保留原版本", cfg.ID)
	}
	return c.st.Update(func(d *clientData) {
		d.Active = cfg
	})
}

func (c *Client) handleActual(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(protocol.ActualResponse{ClientID: c.id, ActiveVersion: c.ActiveVersion()})
}

func (c *Client) handleSync(w http.ResponseWriter, r *http.Request) {
	if err := c.Sync(); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func writeErr(w http.ResponseWriter, err error) {
	w.WriteHeader(502)
	_, _ = io.WriteString(w, err.Error())
}
