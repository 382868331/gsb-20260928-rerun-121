package client

import (
	"os"
	"path/filepath"
	"testing"

	"configdist/internal/protocol"
)

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

func TestValidatorRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	c, _ := New("c1", "http://unused", dir, nil, func(content map[string]interface{}) error {
		return os.ErrInvalid
	})
	rel := &protocol.Release{Version: 1, Content: map[string]interface{}{"x": 1}}
	if reason := c.apply(rel); reason == "" {
		t.Fatal("校验失败应拒绝切换")
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
