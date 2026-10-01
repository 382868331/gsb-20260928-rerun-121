package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"configdist/internal/protocol"
)

// FailHook 允许演示注入“应用失败”：返回非空原因即视为应用失败。
type FailHook func(version int, content map[string]interface{}) string

// Validator 允许上层对整份配置做业务校验，返回错误则拒绝切换。
type Validator func(content map[string]interface{}) error

// AckHook 返回 true 时丢弃该确认，用于模拟确认丢失。
type AckHook func(version int, kind string) bool

type Client struct {
	ID        string
	serverURL string
	dataDir   string
	httpc     *http.Client
	failHook  FailHook
	validator Validator
	ackHook   AckHook

	mu            sync.Mutex
	activeVersion int
	paused        bool
	stop          chan struct{}
}

func New(id, serverURL, dataDir string, failHook FailHook, validator Validator) (*Client, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	c := &Client{
		ID:        id,
		serverURL: serverURL,
		dataDir:   dataDir,
		httpc:     &http.Client{Timeout: 5 * time.Second},
		failHook:  failHook,
		validator: validator,
		stop:      make(chan struct{}),
	}
	c.activeVersion = c.loadActiveVersion()
	return c, nil
}

func (c *Client) ActiveVersion() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeVersion
}

// SetServerURL 在服务端重启更换地址后重新指向。
func (c *Client) SetServerURL(url string) {
	c.serverURL = url
}

// SetHTTPClient 注入自定义传输（演示中用进程内直连，无需真实端口）。
func (c *Client) SetHTTPClient(hc *http.Client) {
	c.httpc = hc
}

// SetPaused 模拟短暂断连：暂停期间不进行任何心跳/下载。
func (c *Client) SetPaused(p bool) {
	c.mu.Lock()
	c.paused = p
	c.mu.Unlock()
}

func (c *Client) SetAckHook(h AckHook) {
	c.mu.Lock()
	c.ackHook = h
	c.mu.Unlock()
}

func (c *Client) activePath() string  { return filepath.Join(c.dataDir, "active.json") }
func (c *Client) stagingPath() string { return filepath.Join(c.dataDir, "active.json.staging") }
func (c *Client) metaPath() string    { return filepath.Join(c.dataDir, "meta.json") }

type meta struct {
	ActiveVersion int `json:"active_version"`
}

// loadActiveVersion 重启后从磁盘恢复“实际生效版本”。
func (c *Client) loadActiveVersion() int {
	b, err := os.ReadFile(c.metaPath())
	if err != nil {
		return 0
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return 0
	}
	return m.ActiveVersion
}

// commitActiveVersion 与配置切换一起落盘，保证重启后可对账。
func (c *Client) commitActiveVersion(version int) error {
	b, _ := json.MarshalIndent(meta{ActiveVersion: version}, "", "  ")
	tmp := c.metaPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.metaPath())
}

// Start 周期性心跳：上报实际版本，发现落后就拉最新全量并原子切换。
func (c *Client) Start(interval time.Duration) {
	go func() {
		c.SyncOnce()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.SyncOnce()
			}
		}
	}()
}

func (c *Client) Stop() { close(c.stop) }

// SyncOnce 执行一轮：对账 → 下载 → 确认下载 → 原子应用 → 确认应用/上报失败。
func (c *Client) SyncOnce() {
	c.mu.Lock()
	paused := c.paused
	c.mu.Unlock()
	if paused {
		return
	}
	resp, err := c.register()
	if err != nil {
		log.Printf("[%s] 心跳失败（短暂断连，稍后重试）: %v", c.ID, err)
		return
	}
	target := resp.TargetRelease
	if target == nil {
		target = resp.Latest
	}
	if target == nil || target.Version == c.ActiveVersion() {
		return // 已处于服务端授权目标
	}
	rel := target

	// 直接取授权目标完整版本；升级、跳级或显式回退都走同一条原子切换路径。
	full, err := c.download(rel.Version)
	if err != nil {
		log.Printf("[%s] 下载 v%d 失败: %v", c.ID, rel.Version, err)
		return
	}
	// 下载确认：明确告诉服务端“只下载了”，绝不等于应用成功。
	if err := c.ack(full.Version, protocol.AckDownloaded, ""); err != nil {
		log.Printf("[%s] downloaded 确认失败: %v", c.ID, err)
	}
	if Checksum(full.Content) != full.Checksum {
		c.reportFailure(full.Version, "校验和不一致，拒绝应用")
		return
	}
	if reason := c.validate(full); reason != "" {
		c.reportFailure(full.Version, reason)
		return
	}
	if err := c.ack(full.Version, protocol.AckValidated, ""); err != nil {
		log.Printf("[%s] validated 确认失败: %v", c.ID, err)
	}

	if reason := c.switchConfig(full); reason != "" {
		// 应用失败：active.json 保持原版不变，上报失败原因，状态留待后续。
		log.Printf("[%s] 应用 v%d 失败，保留 v%d: %s", c.ID, full.Version, c.ActiveVersion(), reason)
		if err := c.ack(full.Version, protocol.AckFailed, reason); err != nil {
			log.Printf("[%s] failed 确认失败: %v", c.ID, err)
		}
		return
	}

	c.mu.Lock()
	c.activeVersion = full.Version
	c.mu.Unlock()
	if err := c.commitActiveVersion(full.Version); err != nil {
		log.Printf("[%s] 元数据落盘失败: %v", c.ID, err)
	}
	log.Printf("[%s] 已切换到 v%d", c.ID, full.Version)
	if err := c.ack(full.Version, protocol.AckApplied, ""); err != nil {
		log.Printf("[%s] applied 确认失败: %v", c.ID, err)
	}
}

// apply 执行原子切换：写暂存 → 校验 → rename 覆盖。
// 任何一步失败都返回原因，原 active.json 原样保留。
func (c *Client) apply(rel *protocol.Release) string {
	if reason := c.validate(rel); reason != "" {
		return reason
	}
	return c.switchConfig(rel)
}

func (c *Client) validate(rel *protocol.Release) string {
	if c.validator != nil {
		if err := c.validator(rel.Content); err != nil {
			return "配置校验失败: " + err.Error()
		}
	}
	return ""
}

func (c *Client) switchConfig(rel *protocol.Release) string {
	if c.failHook != nil {
		if reason := c.failHook(rel.Version, rel.Content); reason != "" {
			return reason
		}
	}
	b, err := json.MarshalIndent(rel.Content, "", "  ")
	if err != nil {
		return "编码失败: " + err.Error()
	}
	// 先写暂存文件并落盘。
	if err := os.WriteFile(c.stagingPath(), b, 0o644); err != nil {
		return "写暂存失败: " + err.Error()
	}
	// 再原子改名覆盖当前生效配置；失败时 active.json 不受影响。
	if err := os.Rename(c.stagingPath(), c.activePath()); err != nil {
		return "原子切换失败: " + err.Error()
	}
	return ""
}

func (c *Client) register() (*protocol.ReconcileResponse, error) {
	body, _ := json.Marshal(protocol.RegisterRequest{
		ClientID:      c.ID,
		ActiveVersion: c.ActiveVersion(),
	})
	resp, err := c.post("/api/v1/register", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out protocol.ReconcileResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) download(version int) (*protocol.Release, error) {
	resp, err := c.httpc.Get(c.serverURL + "/api/v1/releases/" + fmt.Sprint(version))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("下载返回 %d: %s", resp.StatusCode, b)
	}
	var rel protocol.Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func (c *Client) reportFailure(version int, reason string) {
	if err := c.ack(version, protocol.AckFailed, reason); err != nil {
		log.Printf("[%s] failed 确认失败: %v", c.ID, err)
	}
}

func (c *Client) ack(version int, kind, reason string) error {
	c.mu.Lock()
	drop := c.ackHook != nil && c.ackHook(version, kind)
	c.mu.Unlock()
	if drop {
		log.Printf("[%s] v%d %s 确认模拟丢失", c.ID, version, kind)
		return nil
	}
	body, _ := json.Marshal(protocol.AckRequest{
		ClientID: c.ID,
		Version:  version,
		Kind:     kind,
		Reason:   reason,
	})
	resp, err := c.post("/api/v1/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ack %s 返回 %d: %s", kind, resp.StatusCode, b)
	}
	return nil
}

func (c *Client) post(path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.serverURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpc.Do(req)
}

// Checksum 与服务端一致：规范化 JSON 后 sha256。
func Checksum(v map[string]interface{}) string {
	b, _ := json.Marshal(canonical(v))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func canonical(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := map[string]interface{}{}
		for _, k := range keys {
			out[k] = canonical(t[k])
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i := range t {
			out[i] = canonical(t[i])
		}
		return out
	default:
		return v
	}
}
