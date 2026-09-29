package server

import (
	"testing"

	"configdist/internal/protocol"
)

func TestPublishAndAckStateMonotonic(t *testing.T) {
	store := newTestStore(t)

	rel, err := store.Publish(map[string]interface{}{"k": "v1"})
	if err != nil || rel.Version != 1 {
		t.Fatalf("发布 v1 失败: %v", err)
	}
	if _, _, err := store.Touch("c1", 0); err != nil {
		t.Fatal(err)
	}

	// downloaded 不算成功：active 仍是 0，状态 downloaded。
	if err := store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckDownloaded}); err != nil {
		t.Fatal(err)
	}
	if got := clientOf(t, store, "c1"); got.State != protocol.StateDownloaded || got.ActiveVersion != 0 {
		t.Fatalf("downloaded 后状态错误: %+v", got)
	}

	// applied 才推进实际版本。
	if err := store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied}); err != nil {
		t.Fatal(err)
	}
	if got := clientOf(t, store, "c1"); got.State != protocol.StateApplied || got.ActiveVersion != 1 {
		t.Fatalf("applied 后状态错误: %+v", got)
	}

	// 重复 / 乱序确认不能回退。
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckDownloaded})
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckFailed, Reason: "x"})
	if got := clientOf(t, store, "c1"); got.State != protocol.StateApplied || got.LastError != "" {
		t.Fatalf("applied 不应被回退: %+v", got)
	}
}

func TestFailureKeepsOriginalAndStaysPendingForNewVersion(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"})
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied})

	_, _ = store.Publish(map[string]interface{}{"k": "v2"})
	// 心跳对账：实际仍在 v1。
	_, _, _ = store.Touch("c1", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckFailed, Reason: "boom"})
	got := clientOf(t, store, "c1")
	if got.ActiveVersion != 1 || got.State != protocol.StateFailed || got.LastError != "boom" {
		t.Fatalf("失败后应保留 v1 与原因: %+v", got)
	}
}

func TestRestartReconcile(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"})
	_, _ = store.Publish(map[string]interface{}{"k": "v2"})
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckDownloaded})

	// 用同一目录“重启”：下载态不应被当成已应用。
	store2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := clientOf(t, store2, "c1"); got.ActiveVersion != 0 || got.State != protocol.StateDownloaded {
		t.Fatalf("重启后状态应保留为 downloaded: %+v", got)
	}
	// 客户端心跳自报实际 v1（应用没成功），应回到待处理而非误判 applied。
	_, _, _ = store2.Touch("c1", 1)
	if got := clientOf(t, store2, "c1"); got.ActiveVersion != 1 || got.State == protocol.StateApplied {
		t.Fatalf("对账不应臆测已应用: %+v", got)
	}
	// 自报已到 v2，则以实际为准修正为 applied。
	_, _, _ = store2.Touch("c1", 2)
	if got := clientOf(t, store2, "c1"); got.State != protocol.StateApplied || got.ActiveVersion != 2 {
		t.Fatalf("实际已最新应修正 applied: %+v", got)
	}
}

func TestOldVersionAckIgnored(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"})
	_, _ = store.Publish(map[string]interface{}{"k": "v2"})
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied}) // 旧版本
	if got := clientOf(t, store, "c1"); got.ActiveVersion != 0 {
		t.Fatalf("旧版本 applied 不应推进当前实际版本: %+v", got)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func clientOf(t *testing.T, s *Store, id string) clientState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		t.Fatalf("客户端 %s 不存在", id)
	}
	return *c
}
