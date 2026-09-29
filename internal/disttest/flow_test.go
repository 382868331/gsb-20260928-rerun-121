package disttest

import (
	"testing"
	"time"

	"configdist/internal/protocol"
)

// 1. 发布后客户端自动取最新完整版本，即便“错过”中间版本。
func TestPublishAndApplyLatestFullSnapshot(t *testing.T) {
	h := setup(t)
	c := h.startClient(t, "c1")

	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return c.ActiveVersion() == "v1" })

	// 不发布/不等待 v2，直接发布 v3：客户端直接获得 v3 完整快照。
	h.publish(t, "v3", 3, map[string]any{"k": 3})
	waitFor(t, func() bool { return c.ActiveVersion() == "v3" })

	cs := h.status(t).Clients["c1"]
	if cs.AppliedID != "v3" || cs.PendingID != "" {
		t.Fatalf("applied=%q pending=%q", cs.AppliedID, cs.PendingID)
	}
}

// 2. 下载确认不是应用成功。
func TestDownloadedIsNotSuccess(t *testing.T) {
	h := setup(t)
	h.startClient(t, "c1")
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return h.status(t).Clients["c1"].AppliedID == "v1" })

	h.publish(t, "v2", 2, map[string]any{"k": 2})
	h.report(t, protocol.ReportRequest{ClientID: "c1", Stage: protocol.StageDownloaded, VersionID: "v2"})

	cs := h.status(t).Clients["c1"]
	if cs.AppliedID != "v1" {
		t.Fatalf("仅下载不应改变 applied，got %q", cs.AppliedID)
	}
	if cs.PendingID != "v2" || cs.DownloadedID != "v2" {
		t.Fatalf("pending=%q downloaded=%q", cs.PendingID, cs.DownloadedID)
	}
}

// 3. 应用失败保留原版，服务端保留原因与待处理；恢复后自动追上。
func TestApplyFailureKeepsOriginalThenRecovers(t *testing.T) {
	h := setup(t)
	c := h.startClient(t, "c1")
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return c.ActiveVersion() == "v1" })

	c.SetFailVersion("v2")
	h.publish(t, "v2", 2, map[string]any{"k": 2})
	waitFor(t, func() bool {
		cs := h.status(t).Clients["c1"]
		return cs.FailureReason != "" && cs.PendingID == "v2"
	})
	if c.ActiveVersion() != "v1" {
		t.Fatalf("失败后应保留原版本，got %q", c.ActiveVersion())
	}

	c.SetFailVersion("")
	waitFor(t, func() bool {
		cs := h.status(t).Clients["c1"]
		return cs.AppliedID == "v2" && cs.PendingID == "" && cs.FailureReason == ""
	})
}

// 4. 重复/更旧的确认不能让状态回退。
func TestStaleConfirmationCannotRollBack(t *testing.T) {
	h := setup(t)
	c := h.startClient(t, "c1")
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	h.publish(t, "v2", 2, map[string]any{"k": 2})
	waitFor(t, func() bool { return c.ActiveVersion() == "v2" })

	h.report(t, protocol.ReportRequest{ClientID: "c1", Stage: protocol.StageApplied, VersionID: "v1"})
	if cs := h.status(t).Clients["c1"]; cs.AppliedID != "v2" {
		t.Fatalf("旧确认导致回退: %q", cs.AppliedID)
	}

	h.report(t, protocol.ReportRequest{ClientID: "c1", Stage: protocol.StageApplied, VersionID: "v2"})
	cs := h.status(t).Clients["c1"]
	if cs.AppliedID != "v2" || cs.PendingID != "" {
		t.Fatalf("重复确认后状态异常: applied=%q pending=%q", cs.AppliedID, cs.PendingID)
	}
}

// 5. 短暂断开：离线时 reconcile 标记不可达；恢复后以实际版本为准。
func TestReconcileReflectsActualVersion(t *testing.T) {
	h := setup(t)
	c := h.startClient(t, "c1")
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return c.ActiveVersion() == "v1" })
	h.publish(t, "v2", 2, map[string]any{"k": 2})
	waitFor(t, func() bool { return c.ActiveVersion() == "v2" })

	h.stopClient(c)
	results := h.reconcile(t)
	if len(results) != 1 || results[0]["online"] != false {
		t.Fatalf("客户端离线时应标记不可达: %v", results)
	}
}

// 6. 服务端重启后从磁盘恢复版本与客户端状态。
func TestServerRestartRestoresState(t *testing.T) {
	h := setup(t)
	c := h.startClient(t, "c1")
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return c.ActiveVersion() == "v1" })

	h.restartServer(t)
	time.Sleep(100 * time.Millisecond)
	st := h.status(t)
	if st.DesiredID != "v1" {
		t.Fatalf("重启后期望版本丢失: %q", st.DesiredID)
	}
	if st.Clients["c1"].AppliedID != "v1" {
		t.Fatalf("重启后客户端应用记录丢失: %+v", st.Clients["c1"])
	}
}

// 7. reconcile 在线时按客户端真实在用版本纠正记录，而不是只信下载确认。
func TestReconcileCorrectsRecordFromActual(t *testing.T) {
	h := setup(t)
	c := h.startClient(t, "c1")
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return c.ActiveVersion() == "v1" })

	// 服务端只收到 downloaded，记录里仍待处理；reconcile 回连 /actual 后纠正。
	h.publish(t, "v2", 2, map[string]any{"k": 2})
	h.report(t, protocol.ReportRequest{ClientID: "c1", Stage: protocol.StageDownloaded, VersionID: "v2"})

	// 让客户端真实应用 v2（真实状态领先于服务端记录）。
	waitFor(t, func() bool { return c.ActiveVersion() == "v2" })
	results := h.reconcile(t)
	if len(results) != 1 || results[0]["activeVersion"] != "v2" {
		t.Fatalf("reconcile 结果异常: %v", results)
	}
	cs := h.status(t).Clients["c1"]
	if cs.AppliedID != "v2" || cs.PendingID != "" {
		t.Fatalf("reconcile 未按实际版本纠正: %+v", cs)
	}
}

// 8. 客户端重启后从磁盘读出实际在用版本，不依赖服务端记忆。
func TestClientRestartKeepsActiveVersion(t *testing.T) {
	h := setup(t)
	dir := t.TempDir()
	c1 := h.startClientIn(t, "c1", dir)
	h.publish(t, "v1", 1, map[string]any{"k": 1})
	waitFor(t, func() bool { return c1.ActiveVersion() == "v1" })
	h.stopClient(c1)

	c2 := h.startClientIn(t, "c1", dir)
	if c2.ActiveVersion() != "v1" {
		t.Fatalf("客户端重启后实际版本丢失: %q", c2.ActiveVersion())
	}

	h.publish(t, "v2", 2, map[string]any{"k": 2})
	waitFor(t, func() bool { return c2.ActiveVersion() == "v2" })
}
