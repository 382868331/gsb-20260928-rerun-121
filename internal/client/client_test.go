package client

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"configdist/internal/protocol"
	"configdist/internal/server"
)

// handlerClient 不监听真实端口，直接进程内调用服务端 Handler。
type handlerTransport struct{ h http.Handler }

func (t *handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// newTestClient 用进程内传输连接服务端，返回客户端与服务端存储。
func newTestClient(t *testing.T, id string, hook FailHook, v Validator) (*Client, *server.Store) {
	t.Helper()
	store, err := server.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(id, "http://in-process", t.TempDir(), hook, v)
	if err != nil {
		t.Fatal(err)
	}
	c.SetHTTPClient(&http.Client{Transport: &handlerTransport{h: server.New(store).Handler()}})
	return c, store
}

func TestApplyFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	c, err := New("c1", "http://unused", dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	orig := &protocol.Release{Version: 1, Content: map[string]interface{}{"v": 1}, Checksum: Checksum(map[string]interface{}{"v": 1})}
	if reason := c.apply(orig); reason != "" {
		t.Fatalf("v1 应应用成功: %s", reason)
	}
	c.activeVersion = 1
	if err := c.commitActiveVersion(1); err != nil {
		t.Fatal(err)
	}

	// v2 应用失败（故障钩子），active.json 必须保持 v1 内容。
	fail := func(version int, _ map[string]interface{}) string {
		if version == 2 {
			return "boom"
		}
		return ""
	}
	c.failHook = fail
	v2 := &protocol.Release{Version: 2, Content: map[string]interface{}{"v": 2}, Checksum: Checksum(map[string]interface{}{"v": 2})}
	if reason := c.apply(v2); reason != "boom" {
		t.Fatalf("应返回失败原因，得到 %q", reason)
	}
	b, err := os.ReadFile(filepath.Join(dir, "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || !contains(string(b), "1") || contains(string(b), "2") {
		t.Fatalf("失败后 active.json 应仍是 v1，实际: %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "active.json.staging")); !os.IsNotExist(err) {
		t.Fatalf("暂存文件不应残留: %v", err)
	}
}

// 端到端一轮同步：下载 → verified → applied 三阶段确认分别记录。
func TestSyncOnceThreeStageAcks(t *testing.T) {
	c, store := newTestClient(t, "c1", nil, nil)
	if _, err := store.Publish(map[string]interface{}{"x": 1}, nil); err != nil {
		t.Fatal(err)
	}
	c.SyncOnce()
	if c.ActiveVersion() != 1 {
		t.Fatalf("应已切换到 v1，实际 v%d", c.ActiveVersion())
	}
	snap := store.Snapshot(nil)
	recs := snap.History["c1"]
	if len(recs) != 1 || recs[0].State != protocol.StateApplied {
		t.Fatalf("v1 记录应为 applied: %+v", recs)
	}
}

// 业务校验失败：拒绝切换并上报 failed，active.json 保持原版。
func TestValidatorRejectsBadConfig(t *testing.T) {
	c, store := newTestClient(t, "c1", nil, nil)
	_, _ = store.Publish(map[string]interface{}{"x": 1}, nil)
	c.SyncOnce() // v1 正常
	if c.ActiveVersion() != 1 {
		t.Fatal("v1 应生效")
	}
	// v2 业务校验不通过。
	_, _ = store.Publish(map[string]interface{}{"x": 2}, nil)
	c.validator = func(content map[string]interface{}) error { return os.ErrInvalid }
	c.SyncOnce()
	if c.ActiveVersion() != 1 {
		t.Fatalf("校验失败应保留 v1，实际 v%d", c.ActiveVersion())
	}
	for _, v := range store.Snapshot(nil).Clients {
		if v.ClientID == "c1" && (v.State != protocol.StateFailed || v.ActiveVersion != 1) {
			t.Fatalf("服务端应记录 failed 且实际版本保持 v1: %+v", v)
		}
	}
}

// 确认丢失：applied 确认被丢弃，心跳对账按实际生效版本补记。
func TestAckLossReconciled(t *testing.T) {
	c, store := newTestClient(t, "c1", nil, nil)
	_, _ = store.Publish(map[string]interface{}{"x": 1}, nil)
	c.SetAckDropper(func(_ int, kind string) bool { return kind == protocol.AckApplied })
	c.SyncOnce()
	if c.ActiveVersion() != 1 {
		t.Fatal("客户端实际已应用 v1")
	}
	for _, v := range store.Snapshot(nil).Clients {
		if v.State == protocol.StateApplied {
			t.Fatalf("applied 确认已丢失，服务端不应是 applied: %+v", v)
		}
	}
	// 模拟客户端重启：从磁盘恢复实际版本，心跳对账补记 applied。
	c2, _ := New("c1", "http://in-process", c.dataDir, nil, nil)
	c2.SetHTTPClient(c.httpc)
	c2.SyncOnce()
	for _, v := range store.Snapshot(nil).Clients {
		if v.State != protocol.StateApplied || v.ActiveVersion != 1 {
			t.Fatalf("重启对账应补记 applied: %+v", v)
		}
	}
}

func TestActiveVersionPersistence(t *testing.T) {
	dir := t.TempDir()
	c, _ := New("c1", "http://unused", dir, nil, nil)
	rel := &protocol.Release{Version: 3, Content: map[string]interface{}{"v": 3}, Checksum: Checksum(map[string]interface{}{"v": 3})}
	if r := c.apply(rel); r != "" {
		t.Fatalf("应用失败: %s", r)
	}
	if err := c.commitActiveVersion(3); err != nil {
		t.Fatal(err)
	}
	// 模拟重启：从磁盘恢复实际版本。
	c2, err := New("c1", "http://unused", dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.ActiveVersion() != 3 {
		t.Fatalf("重启后应恢复实际版本 3，得到 %d", c2.ActiveVersion())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
