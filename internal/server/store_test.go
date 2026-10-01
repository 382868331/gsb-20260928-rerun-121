package server

import (
	"testing"

	"configdist/internal/protocol"
)

func TestPublishAndAckStateMonotonic(t *testing.T) {
	store := newTestStore(t)

	rel, err := store.Publish(protocol.PublishRequest{Content: map[string]interface{}{"k": "v1"}})
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
	publishTest(store, map[string]interface{}{"k": "v1"})
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied})

	publishTest(store, map[string]interface{}{"k": "v2"})
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
	publishTest(store, map[string]interface{}{"k": "v1"})
	publishTest(store, map[string]interface{}{"k": "v2"})
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
	publishTest(store, map[string]interface{}{"k": "v1"})
	publishTest(store, map[string]interface{}{"k": "v2"})
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied}) // 旧版本
	if got := clientOf(t, store, "c1"); got.ActiveVersion != 0 {
		t.Fatalf("旧版本 applied 不应推进当前实际版本: %+v", got)
	}
}

func TestStagedRolloutDownloadAndValidationDoNotPromote(t *testing.T) {
	store := newTestStore(t)
	publishTest(store, map[string]interface{}{"k": "v1"})
	_, _, _ = store.Touch("canary", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "canary", Version: 1, Kind: protocol.AckApplied})

	_, _ = store.Publish(protocol.PublishRequest{
		Content:        map[string]interface{}{"k": "v2"},
		BatchClientIDs: []string{"canary"},
	})
	_, _, _ = store.Touch("canary", 1)
	_, _, _ = store.Touch("next", 1)
	if _, has, _ := store.Touch("next", 1); has {
		t.Fatal("后续批次不应拿到 v2 快照")
	}

	_ = store.Ack(protocol.AckRequest{ClientID: "canary", Version: 2, Kind: protocol.AckDownloaded})
	if got := clientOf(t, store, "canary"); got.State != protocol.StateDownloaded || got.ActiveVersion != 1 {
		t.Fatalf("downloaded 不应推进实际版本: %+v", got)
	}
	if err := store.PromoteBatch(2, []string{"next"}); err != nil {
		t.Fatalf("仅下载成功不应阻止后续批次: %v", err)
	}

	_, _, _ = store.Touch("next", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "next", Version: 2, Kind: protocol.AckDownloaded})
	_ = store.Ack(protocol.AckRequest{ClientID: "next", Version: 2, Kind: protocol.AckValidated})
	if got := clientOf(t, store, "next"); got.State != protocol.StateValidated || got.ActiveVersion != 1 {
		t.Fatalf("validated 不是 applied: %+v", got)
	}
	if err := store.PromoteBatch(2, []string{"later"}); err != nil {
		t.Fatalf("校验成功但未真正应用时不应自动视为成功: %v", err)
	}
}

func TestApplicationFailureStopsLaterBatchesAndRollbackPreservesHistory(t *testing.T) {
	store := newTestStore(t)
	publishTest(store, map[string]interface{}{"k": "v1"})
	for _, id := range []string{"canary", "next"} {
		_, _, _ = store.Touch(id, 0)
		_ = store.Ack(protocol.AckRequest{ClientID: id, Version: 1, Kind: protocol.AckApplied})
	}

	_, _ = store.Publish(protocol.PublishRequest{
		Content:        map[string]interface{}{"k": "v2"},
		BatchClientIDs: []string{"canary"},
	})
	_, _, _ = store.Touch("canary", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "canary", Version: 2, Kind: protocol.AckDownloaded})
	_ = store.Ack(protocol.AckRequest{ClientID: "canary", Version: 2, Kind: protocol.AckValidated})
	_ = store.Ack(protocol.AckRequest{ClientID: "canary", Version: 2, Kind: protocol.AckFailed, Reason: "boom"})

	got := clientOf(t, store, "canary")
	if got.ActiveVersion != 1 || got.State != protocol.StateFailed || len(got.Failures) != 1 {
		t.Fatalf("失败后应保留 v1 和失败历史: %+v", got)
	}
	if err := store.PromoteBatch(2, []string{"next"}); err == nil {
		t.Fatal("首批应用失败后必须停止后续批次")
	}

	if _, err := store.Rollback(1); err != nil {
		t.Fatal(err)
	}
	_, _, _ = store.Touch("canary", 1)
	got = clientOf(t, store, "canary")
	if got.ActiveVersion != 1 || got.State != protocol.StateApplied || len(got.Failures) != 1 {
		t.Fatalf("回退到旧版不得覆盖失败历史: %+v", got)
	}
	if err := store.Ack(protocol.AckRequest{ClientID: "canary", Version: 2, Kind: protocol.AckApplied}); err == nil {
		t.Fatal("回退后迟到的 v2 applied 必须被拒绝")
	}
	got = clientOf(t, store, "canary")
	if got.ActiveVersion != 1 || got.State != protocol.StateApplied {
		t.Fatalf("迟到旧确认不得改标新版本: %+v", got)
	}
}

func publishTest(s *Store, content map[string]interface{}) protocol.Release {
	rel, err := s.Publish(protocol.PublishRequest{Content: content})
	if err != nil {
		panic(err)
	}
	return rel
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
