package combo

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "srv"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func publishBase(t *testing.T, s *Store) {
	t.Helper()
	for _, v := range []int{1, 2, 3} {
		if err := s.PublishVersion("firmware", v); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []int{1, 2} {
		if err := s.PublishVersion("rf-driver", v); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []int{4, 5} {
		if err := s.PublishVersion("billing-app", v); err != nil {
			t.Fatal(err)
		}
	}
	s.SetRules([]Rule{
		{ID: "R1", When: Condition{"rf-driver", 2}, Require: Condition{"firmware", 3}},
		{ID: "R2", When: Condition{"billing-app", 5}, Require: Condition{"rf-driver", 2}},
	}, "交叉依赖")
}

func register(t *testing.T, s *Store, id string, cur Combination) {
	t.Helper()
	if _, err := s.Register(RegisterRequest{DeviceID: id, Current: cur}); err != nil {
		t.Fatal(err)
	}
}

func TestCrossDependencyBlocksPlan(t *testing.T) {
	s := newTestStore(t)
	publishBase(t, s)
	register(t, s, "dev-1", Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4})

	bad := s.Plan(Combination{"firmware": 1, "rf-driver": 2, "billing-app": 5})
	if bad.Compatible {
		t.Fatal("违反交叉依赖的组合不应兼容")
	}
	if len(bad.Problems) != 1 { // R1 被违反
		t.Fatalf("期望 1 条阻碍原因, 实际 %v", bad.Problems)
	}
	good := s.Plan(Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5})
	if !good.Compatible {
		t.Fatalf("兼容组合被误判: %v", good.Problems)
	}
	if _, err := s.Release(ReleaseRequest{Target: Combination{"firmware": 1, "rf-driver": 2, "billing-app": 5}}); err == nil {
		t.Fatal("不兼容组合不应允许发布")
	}
}

func TestDownloadedDoesNotMeanUsable(t *testing.T) {
	s := newTestStore(t)
	publishBase(t, s)
	target := Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	register(t, s, "dev-1", Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4})
	dec, err := s.Release(ReleaseRequest{Target: target, DeviceIDs: []string{"dev-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, comp := range []string{"firmware", "rf-driver", "billing-app"} {
		if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: comp, Kind: AckDownloaded}); err != nil {
			t.Fatal(err)
		}
		if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: comp, Kind: AckValidated}); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Snapshot(nil)
	if st.Devices[0].Done {
		t.Fatal("仅 downloaded/validated 不应视为组合可用")
	}
	if got := len(st.Devices[0].WaitingOn); got != 3 {
		t.Fatalf("期望 3 项待收尾, 实际 %d", got)
	}
	for _, comp := range []string{"firmware", "rf-driver"} {
		_ = s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: comp, Kind: AckApplied})
	}
	st = s.Snapshot(nil)
	if st.Devices[0].Done {
		t.Fatal("部分组件 applied 不应视为整组完成")
	}
	if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "billing-app", Kind: AckApplied}); err != nil {
		t.Fatal(err)
	}
	st = s.Snapshot(nil)
	if !st.Devices[0].Done {
		t.Fatal("全部组件 applied 后整组应完成")
	}
	if !st.Devices[0].Current.Equal(target) {
		t.Fatalf("完成后实际组合应为目标组合, 实际 %v", st.Devices[0].Current)
	}
}

func TestWithdrawReevaluatesDevices(t *testing.T) {
	s := newTestStore(t)
	publishBase(t, s)
	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	target := Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	register(t, s, "dev-done", old)
	register(t, s, "dev-partial", old)
	register(t, s, "dev-fresh", old)
	dec, err := s.Release(ReleaseRequest{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	cid := dec.CampaignID
	// dev-done 全部应用完成
	for _, comp := range []string{"firmware", "rf-driver", "billing-app"} {
		_ = s.Ack(AckRequest{DeviceID: "dev-done", CampaignID: cid, Component: comp, Kind: AckApplied})
	}
	// dev-partial 只应用了 firmware
	_ = s.Ack(AckRequest{DeviceID: "dev-partial", CampaignID: cid, Component: "firmware", Kind: AckApplied})
	// dev-fresh 未开始

	if err := s.WithdrawVersion("rf-driver", 2); err != nil {
		t.Fatal(err)
	}
	st := s.Snapshot(nil)
	views := map[string]DeviceView{}
	for _, v := range st.Devices {
		views[v.DeviceID] = v
	}
	if !views["dev-done"].Done || !views["dev-done"].Current.Equal(target) {
		t.Fatal("已完成设备必须保留真实状态，不受撤回影响")
	}
	if len(views["dev-fresh"].Blocked) == 0 {
		t.Fatal("未开始设备应被重新核对并给出阻碍原因")
	}
	if len(views["dev-partial"].Blocked) != 0 {
		t.Fatal("部分应用设备不重新核对，继续等待收尾项")
	}
	if got := views["dev-partial"].WaitingOn; len(got) != 2 {
		t.Fatalf("部分应用设备应明确等待 2 项收尾, 实际 %v", got)
	}
}

func TestRollbackMustBeCompatibleCombination(t *testing.T) {
	s := newTestStore(t)
	publishBase(t, s)
	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	register(t, s, "dev-1", old)
	target := Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	if _, err := s.Release(ReleaseRequest{Target: target}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawVersion("rf-driver", 2); err != nil {
		t.Fatal(err)
	}
	// 含已撤回版本的回退组合被拒绝
	if _, err := s.Rollback(RollbackRequest{Target: Combination{"firmware": 1, "rf-driver": 2, "billing-app": 4}}); err == nil {
		t.Fatal("含已撤回版本的回退组合不应被接受")
	}
	// 仍兼容的整组组合可以回退
	if _, err := s.Rollback(RollbackRequest{Target: old}); err != nil {
		t.Fatalf("兼容组合回退失败: %v", err)
	}
}

func TestLateAckRejected(t *testing.T) {
	s := newTestStore(t)
	publishBase(t, s)
	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	register(t, s, "dev-1", old)
	dec, _ := s.Release(ReleaseRequest{Target: Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}})
	if _, err := s.Rollback(RollbackRequest{Target: old}); err != nil {
		t.Fatal(err)
	}
	err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied})
	if err == nil {
		t.Fatal("旧行动的迟到确认不应被接受")
	}
}

func TestRollbackPreservesAppliedFacts(t *testing.T) {
	s := newTestStore(t)
	publishBase(t, s)
	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	target := Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	register(t, s, "dev-1", old)
	dec, err := s.Release(ReleaseRequest{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	// 只应用了 firmware 后开始回退
	if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied}); err != nil {
		t.Fatal(err)
	}
	rb, err := s.Rollback(RollbackRequest{Target: old})
	if err != nil {
		t.Fatal(err)
	}
	st := s.Snapshot(nil)
	v := st.Devices[0]
	// 实际组合保留每个组件的真实应用版本：firmware 仍是新版 v3
	want := Combination{"firmware": 3, "rf-driver": 1, "billing-app": 4}
	if !v.Current.Equal(want) {
		t.Fatalf("回退不应擦掉已生效的组件版本, 实际组合 %v", v.Current)
	}
	if v.Done {
		t.Fatal("整组尚未切回旧组合，不应视为完成")
	}
	// 只等待确实需要变化的组件（firmware），其余保持已生效事实
	if got := v.WaitingOn; len(got) != 1 || got[0] != "firmware(pending)" {
		t.Fatalf("应只等待 firmware 收尾, 实际 %v", got)
	}
	if v.Components["rf-driver"].State != CompApplied || v.Components["billing-app"].State != CompApplied {
		t.Fatal("未变化的组件应直接记为 applied，不重新等待")
	}
	// 旧行动的迟到确认不能修改新行动
	if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "rf-driver", Kind: AckApplied}); err == nil {
		t.Fatal("旧行动的迟到确认不应被接受")
	}
	// 重新打开状态后仍能解释实际组合
	s2, err := NewStore(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	v2 := s2.Snapshot(nil).Devices[0]
	if !v2.Current.Equal(want) || len(v2.WaitingOn) != 1 {
		t.Fatalf("重启后应保留真实组合与等待项, 实际 %+v", v2)
	}
	// 新行动中 firmware 回退完成后整组才算完成
	if err := s2.Ack(AckRequest{DeviceID: "dev-1", CampaignID: rb.CampaignID, Component: "firmware", Kind: AckApplied}); err != nil {
		t.Fatal(err)
	}
	v3 := s2.Snapshot(nil).Devices[0]
	if !v3.Done || !v3.Current.Equal(old) {
		t.Fatalf("回退收尾后应完成并处于旧组合, 实际 %+v", v3)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	publishBase(t, s)
	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	register(t, s, "dev-1", old)
	target := Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	dec, err := s.Release(ReleaseRequest{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied})
	if err := s.WithdrawVersion("rf-driver", 2); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	st := s2.Snapshot(nil)
	if st.Campaign == nil || st.Campaign.ID != dec.CampaignID {
		t.Fatal("重启后行动应保留")
	}
	if len(st.Devices) != 1 || len(st.Devices[0].WaitingOn) != 2 {
		t.Fatalf("重启后部分应用设备的等待项应保留, 实际 %+v", st.Devices)
	}
	if len(s2.Decisions()) != 1 {
		t.Fatal("重启后发布决定历史应保留")
	}
	if got := len(s2.RulesetHistory()); got != 2 {
		t.Fatalf("重启后规则代次历史应保留, 实际 %d 代", got)
	}
}

func setupBatchCampaign(t *testing.T, s *Store) Decision {
	t.Helper()
	publishBase(t, s)
	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	for _, id := range []string{"dev-1", "dev-2", "dev-3"} {
		register(t, s, id, old)
	}
	target := Combination{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	dec, err := s.Release(ReleaseRequest{
		Target: target,
		Batches: []BatchRequest{
			{DeviceIDs: []string{"dev-1"}, Condition: BatchCondition{Kind: "all"}},
			{DeviceIDs: []string{"dev-2", "dev-3"}, Condition: BatchCondition{Kind: "count", Count: 2}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dec
}

func applyAll(t *testing.T, s *Store, device string, campaignID int) {
	t.Helper()
	for _, comp := range []string{"firmware", "rf-driver", "billing-app"} {
		if err := s.Ack(AckRequest{DeviceID: device, CampaignID: campaignID, Component: comp, Kind: AckApplied}); err != nil {
			// 组件在新目标中版本未变时，计划创建即记为 applied，设备可能提前完成。
			if strings.Contains(err.Error(), "已完成当前行动") {
				return
			}
			t.Fatalf("%s %s applied: %v", device, comp, err)
		}
	}
}

func TestBatchAdvanceRequiresCondition(t *testing.T) {
	s := newTestStore(t)
	dec := setupBatchCampaign(t, s)

	// 条件未满足时推进被拒。
	if _, err := s.AdvanceBatch(); err == nil {
		t.Fatal("试运行批未完成时不应允许推进")
	}
	// 后续批次设备心跳拿不到目标（上一批完成不代表本批开始）。
	resp, err := s.Register(RegisterRequest{DeviceID: "dev-2", Current: Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Target != nil {
		t.Fatal("批次未推进前 dev-2 不应收到目标")
	}
	// 后续批次设备的确认按迟到处理。
	if err := s.Ack(AckRequest{DeviceID: "dev-2", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied}); err == nil {
		t.Fatal("批次未推进前 dev-2 的确认应被拒绝")
	}

	applyAll(t, s, "dev-1", dec.CampaignID)
	adv, err := s.AdvanceBatch()
	if err != nil {
		t.Fatal(err)
	}
	if adv.Batch != 0 {
		t.Fatalf("推进决定应记录批次 0, 实际 %d", adv.Batch)
	}
	st := s.Snapshot(nil)
	if len(st.Batches) != 2 || st.Batches[0].State != BatchDone || st.Batches[1].State != BatchActive {
		t.Fatalf("批次状态错误: %+v", st.Batches)
	}
	// 各批独立展示：第二批设备仍 pending，不带第一批的完成状态。
	for _, dv := range st.Batches[1].Devices {
		if dv.Done || len(dv.WaitingOn) != 3 {
			t.Fatalf("第二批设备应独立处于待推进状态: %+v", dv)
		}
	}
	// 推进后 dev-1 的迟到确认（旧批次）被拒绝。
	if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied}); err == nil {
		t.Fatal("上一批设备的迟到确认不应越过当前批次")
	}
	// 第二批满足 count=2 后行动完成。
	applyAll(t, s, "dev-2", dec.CampaignID)
	applyAll(t, s, "dev-3", dec.CampaignID)
	if _, err := s.AdvanceBatch(); err != nil {
		t.Fatal(err)
	}
	if st := s.Snapshot(nil); st.Campaign != nil {
		t.Fatal("全部批次完成后行动应结束")
	}
}

func TestPauseBlocksAdvance(t *testing.T) {
	s := newTestStore(t)
	dec := setupBatchCampaign(t, s)
	applyAll(t, s, "dev-1", dec.CampaignID)

	if _, err := s.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceBatch(); err == nil {
		t.Fatal("暂停推进期间不应允许进入下一批")
	}
	if _, err := s.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceBatch(); err != nil {
		t.Fatal("恢复后应允许推进")
	}
}

func TestRollbackFailedOnly(t *testing.T) {
	s := newTestStore(t)
	dec := setupBatchCampaign(t, s)
	// dev-1 部分应用后 billing-app 失败。
	_ = s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied})
	_ = s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "rf-driver", Kind: AckApplied})
	if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "billing-app", Kind: AckFailed, Reason: "校验失败"}); err != nil {
		t.Fatal(err)
	}

	// 失败原因在本批视图中独立展示。
	st0 := s.Snapshot(nil)
	if len(st0.Batches[0].Devices[0].Failed) != 1 {
		t.Fatalf("dev-1 的失败原因应展示, 实际 %v", st0.Batches[0].Devices[0].Failed)
	}

	old := Combination{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	rb, err := s.Rollback(RollbackRequest{Target: old, FailedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rb.Accepted) != 1 || rb.Accepted[0] != "dev-1" {
		t.Fatalf("只回退失败设备应只包含 dev-1, 实际 %v", rb.Accepted)
	}
	// 回退后旧行动的迟到确认不得改变已回退的事实。
	if err := s.Ack(AckRequest{DeviceID: "dev-1", CampaignID: dec.CampaignID, Component: "billing-app", Kind: AckApplied}); err == nil {
		t.Fatal("旧行动的迟到确认应被拒绝")
	}
	applyAll(t, s, "dev-1", rb.CampaignID)
	if _, err := s.AdvanceBatch(); err != nil {
		t.Fatal(err)
	}
	// dev-2/dev-3 不在回退范围内，保持原组合继续服务。
	for _, dv := range s.Snapshot(nil).Devices {
		if dv.DeviceID == "dev-1" {
			if !dv.Current.Equal(old) {
				t.Fatalf("dev-1 应回退到旧组合, 实际 %v", dv.Current)
			}
		}
	}
}

func TestBatchStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	dec := setupBatchCampaign(t, s)
	applyAll(t, s, "dev-1", dec.CampaignID)
	if _, err := s.AdvanceBatch(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	_ = s.Ack(AckRequest{DeviceID: "dev-2", CampaignID: dec.CampaignID, Component: "firmware", Kind: AckApplied})

	s2, err := NewStore(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	st := s2.Snapshot(nil)
	if st.Campaign == nil || st.Campaign.ActiveBatch != 1 || !st.Paused {
		t.Fatalf("重启后当前批次与暂停状态应保留: %+v", st.Campaign)
	}
	if st.Batches[0].State != BatchDone || st.Batches[1].State != BatchActive {
		t.Fatalf("重启后各批决定应保留: %+v", st.Batches)
	}
	// dev-2 的实际确认保留；dev-3 仍待推进。
	var d2, d3 DeviceView
	for _, dv := range st.Batches[1].Devices {
		switch dv.DeviceID {
		case "dev-2":
			d2 = dv
		case "dev-3":
			d3 = dv
		}
	}
	if d2.Current["firmware"] != 3 || len(d2.WaitingOn) != 2 {
		t.Fatalf("重启后 dev-2 的实际确认应保留: %+v", d2)
	}
	if len(d3.WaitingOn) != 3 {
		t.Fatalf("重启后 dev-3 应仍待推进: %+v", d3)
	}
	// 暂停状态持久化：恢复前不能推进。
	if _, err := s2.AdvanceBatch(); err == nil {
		t.Fatal("重启后暂停状态应继续生效")
	}
	// 决定历史包含 release / advance / pause。
	var kinds []string
	for _, d := range s2.Decisions() {
		kinds = append(kinds, d.Kind)
	}
	want := []string{"release", "advance", "pause"}
	if len(kinds) != len(want) {
		t.Fatalf("决定历史应为 %v, 实际 %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("决定历史应为 %v, 实际 %v", want, kinds)
		}
	}
}
