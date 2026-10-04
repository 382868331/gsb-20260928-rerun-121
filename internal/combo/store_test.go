package combo

import (
	"path/filepath"
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
