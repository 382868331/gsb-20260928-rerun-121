package server

import (
	"testing"

	"configdist/internal/protocol"
)

func TestPublishAndAckStateMonotonic(t *testing.T) {
	store := newTestStore(t)

	rel, err := store.Publish(map[string]interface{}{"k": "v1"}, nil)
	if err != nil || rel.Version != 1 {
		t.Fatalf("发布 v1 失败: %v", err)
	}
	if _, _, err := store.Touch("c1", 0); err != nil {
		t.Fatal(err)
	}

	// downloaded / verified 都不算成功：active 仍是 0。
	for _, kind := range []string{protocol.AckDownloaded, protocol.AckVerified} {
		if err := store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: kind}); err != nil {
			t.Fatal(err)
		}
		if got := viewOf(t, store, "c1"); got.State != kind || got.ActiveVersion != 0 {
			t.Fatalf("%s 后状态错误: %+v", kind, got)
		}
	}

	// applied 才推进实际版本。
	if err := store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied}); err != nil {
		t.Fatal(err)
	}
	if got := viewOf(t, store, "c1"); got.State != protocol.StateApplied || got.ActiveVersion != 1 {
		t.Fatalf("applied 后状态错误: %+v", got)
	}

	// 重复 / 乱序确认不能回退。
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckDownloaded})
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckVerified})
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckFailed, Reason: "x"})
	if got := viewOf(t, store, "c1"); got.State != protocol.StateApplied || got.LastError != "" {
		t.Fatalf("applied 不应被回退: %+v", got)
	}
}

func TestFailureKeepsOriginalAndStaysPendingForNewVersion(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"}, nil)
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied})

	_, _ = store.Publish(map[string]interface{}{"k": "v2"}, nil)
	// 心跳对账：实际仍在 v1。
	_, _, _ = store.Touch("c1", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckFailed, Reason: "boom"})
	got := viewOf(t, store, "c1")
	if got.ActiveVersion != 1 || got.State != protocol.StateFailed || got.LastError != "boom" {
		t.Fatalf("失败后应保留 v1 与原因: %+v", got)
	}
}

func TestRestartReconcile(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"}, nil)
	_, _ = store.Publish(map[string]interface{}{"k": "v2"}, nil)
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckDownloaded})

	// 用同一目录“重启”：下载态不应被当成已应用。
	store2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := viewOf(t, store2, "c1"); got.ActiveVersion != 0 || got.State != protocol.StateDownloaded {
		t.Fatalf("重启后状态应保留为 downloaded: %+v", got)
	}
	// 客户端心跳自报实际 v1（应用没成功），应回到待处理而非误判 applied。
	_, _, _ = store2.Touch("c1", 1)
	if got := viewOf(t, store2, "c1"); got.ActiveVersion != 1 || got.State == protocol.StateApplied {
		t.Fatalf("对账不应臆测已应用: %+v", got)
	}
	// 自报已到 v2，则以实际为准修正为 applied（确认丢失场景的对账补偿）。
	_, _, _ = store2.Touch("c1", 2)
	if got := viewOf(t, store2, "c1"); got.State != protocol.StateApplied || got.ActiveVersion != 2 {
		t.Fatalf("实际已最新应修正 applied: %+v", got)
	}
}

func TestOldVersionAckIgnored(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"}, nil)
	_, _ = store.Publish(map[string]interface{}{"k": "v2"}, nil)
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckApplied}) // 旧版本
	if got := viewOf(t, store, "c1"); got.ActiveVersion != 0 {
		t.Fatalf("旧版本 applied 不应推进当前实际版本: %+v", got)
	}
}

// 分批放量：逐批放行，某批失败后后续批次自动停止。
func TestBatchedRolloutAutoStopsOnFailure(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"}, nil)
	for _, id := range []string{"c1", "c2", "c3"} {
		_, _, _ = store.Touch(id, 0)
		_ = store.Ack(protocol.AckRequest{ClientID: id, Version: 1, Kind: protocol.AckApplied})
	}

	// 三批发布 v2：只有首批目标立即变为 v2。
	_, _ = store.Publish(map[string]interface{}{"k": "v2"}, [][]string{{"c1"}, {"c2"}, {"c3"}})
	if got := viewOf(t, store, "c1"); got.TargetVersion != 2 {
		t.Fatalf("首批 c1 目标应为 v2: %+v", got)
	}
	for _, id := range []string{"c2", "c3"} {
		if got := viewOf(t, store, "c2"); got.TargetVersion != 1 {
			t.Fatalf("%s 未到批次，目标应仍为 v1: %+v", id, got)
		}
	}

	// 仅 downloaded/verified 不能触发下一批放行。
	_, _, _ = store.Touch("c1", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckDownloaded})
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckVerified})
	if got := viewOf(t, store, "c2"); got.TargetVersion != 1 {
		t.Fatalf("下载/校验成功不代表可推广，c2 目标应仍为 v1: %+v", got)
	}

	// c1 applied 后第二批放行。
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckApplied})
	if got := viewOf(t, store, "c2"); got.TargetVersion != 2 {
		t.Fatalf("首批完成后 c2 目标应为 v2: %+v", got)
	}

	// c2 应用失败：保留 v1，放量自动停止，c3 目标保持 v1。
	_, _, _ = store.Touch("c2", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "c2", Version: 2, Kind: protocol.AckFailed, Reason: "端口被占用"})
	if got := viewOf(t, store, "c2"); got.ActiveVersion != 1 || got.State != protocol.StateFailed {
		t.Fatalf("c2 失败应保留 v1: %+v", got)
	}
	if got := viewOf(t, store, "c3"); got.TargetVersion != 1 {
		t.Fatalf("放量应自动停止，c3 目标仍为 v1: %+v", got)
	}
	snap := store.Snapshot(nil)
	if snap.Rollout == nil || snap.Rollout.Status != protocol.RolloutHalted {
		t.Fatalf("放量状态应为 halted: %+v", snap.Rollout)
	}
}

// 回退：明确回退目标，失败历史保留，迟到旧确认不得把客户端重新标成新版本已完成。
func TestRollbackPreservesHistoryAndIgnoresLateAcks(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"}, nil)
	for _, id := range []string{"c1", "c2"} {
		_, _, _ = store.Touch(id, 0)
		_ = store.Ack(protocol.AckRequest{ClientID: id, Version: 1, Kind: protocol.AckApplied})
	}
	_, _ = store.Publish(map[string]interface{}{"k": "v2"}, [][]string{{"c1"}, {"c2"}})
	_, _, _ = store.Touch("c1", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckApplied})
	_, _, _ = store.Touch("c2", 1)
	_ = store.Ack(protocol.AckRequest{ClientID: "c2", Version: 2, Kind: protocol.AckFailed, Reason: "boom"})

	// 发布明确回退目标 v1。
	if err := store.Rollback(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		if got := viewOf(t, store, id); got.TargetVersion != 1 {
			t.Fatalf("回退后 %s 目标应为 v1: %+v", id, got)
		}
	}
	// 失败历史未被覆盖。
	snap := store.Snapshot(nil)
	var failedRec *protocol.VersionRecord
	for i, r := range snap.History["c2"] {
		if r.Version == 2 {
			failedRec = &snap.History["c2"][i]
		}
	}
	if failedRec == nil || failedRec.State != protocol.StateFailed || failedRec.LastError != "boom" {
		t.Fatalf("c2 的 v2 失败历史应保留: %+v", snap.History["c2"])
	}

	// c1 心跳对账：实际仍是 v2，目标 v1，处于回退待处理。
	_, _, _ = store.Touch("c1", 2)
	if got := viewOf(t, store, "c1"); got.State != protocol.StatePending || got.ActiveVersion != 2 {
		t.Fatalf("c1 应处于回退待处理: %+v", got)
	}
	// 迟到的 v2 applied 确认被忽略：不得把回退中的 c1 标成新版本已完成。
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 2, Kind: protocol.AckApplied})
	if got := viewOf(t, store, "c1"); got.TargetVersion != 1 || got.State != protocol.StatePending {
		t.Fatalf("迟到旧确认不应影响回退: %+v", got)
	}
	// 回退目标版本不存在应报错。
	if err := store.Rollback(99); err == nil {
		t.Fatal("回退到不存在的版本应报错")
	}
}

// 确认丢失：客户端实际已应用但 applied 确认丢失，重启后心跳对账补记。
func TestAckLossReconciledByHeartbeat(t *testing.T) {
	store := newTestStore(t)
	_, _ = store.Publish(map[string]interface{}{"k": "v1"}, nil)
	_, _, _ = store.Touch("c1", 0)
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckDownloaded})
	_ = store.Ack(protocol.AckRequest{ClientID: "c1", Version: 1, Kind: protocol.AckVerified})
	// applied 确认丢失；客户端实际已生效 v1，心跳自报。
	_, _, _ = store.Touch("c1", 1)
	if got := viewOf(t, store, "c1"); got.State != protocol.StateApplied || got.ActiveVersion != 1 {
		t.Fatalf("确认丢失应由心跳对账补记 applied: %+v", got)
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

func viewOf(t *testing.T, s *Store, id string) protocol.ClientView {
	t.Helper()
	for _, v := range s.Snapshot(nil).Clients {
		if v.ClientID == id {
			return v
		}
	}
	t.Fatalf("客户端 %s 不存在", id)
	return protocol.ClientView{}
}
