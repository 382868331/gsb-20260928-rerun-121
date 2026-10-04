package compat

import "testing"

func setup(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range [][2]interface{}{{"kernel", 1}, {"kernel", 2}, {"engine", 1}, {"engine", 2}, {"ui", 2}, {"ui", 3}} {
		if err := s.RegisterVersion(v[0].(string), v[1].(int)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetRule(Rule{ID: "r1", Component: "engine", Version: 2, Requires: map[string][]int{"kernel": {2}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRule(Rule{ID: "r2", Component: "ui", Version: 3, Requires: map[string][]int{"engine": {2}}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIncompatibleComboRejected(t *testing.T) {
	s := setup(t)
	if err := s.Heartbeat("d1", Combo{"kernel": 1, "engine": 1, "ui": 2}, true); err != nil {
		t.Fatal(err)
	}
	bad := Combo{"kernel": 1, "engine": 2, "ui": 3}
	if _, err := s.StartCampaign("rollout", bad, []string{"d1"}, ""); err == nil {
		t.Fatal("不兼容组合应被拒绝")
	}
	views, _ := s.Preview(bad, []string{"d1"})
	if views[0].Feasible || len(views[0].Reasons) == 0 {
		t.Fatalf("Preview 应给出阻碍原因: %+v", views[0])
	}
}

func TestSingleDownloadDoesNotCompleteCombo(t *testing.T) {
	s := setup(t)
	_ = s.Heartbeat("d1", Combo{"kernel": 1, "engine": 1, "ui": 2}, true)
	target := Combo{"kernel": 2, "engine": 2, "ui": 3}
	camp, err := s.StartCampaign("rollout", target, []string{"d1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "ui", Kind: CompDownloaded})
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "kernel", Kind: CompApplied})
	devices, _ := s.Status()
	d := devices["d1"]
	if d.Complete {
		t.Fatal("单个组件下载/应用不能宣布组合完成")
	}
	if len(d.WaitingFor) == 0 {
		t.Fatal("部分应用设备应明确等待收尾项")
	}
	if d.Current["kernel"] != 1 {
		t.Fatal("组合未完成前 Current 不应变化")
	}
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "engine", Kind: CompApplied})
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "ui", Kind: CompApplied})
	devices, _ = s.Status()
	if !devices["d1"].Complete || devices["d1"].Current["ui"] != 3 {
		t.Fatal("全部组件 applied 后组合才应生效")
	}
}

func TestWithdrawRechecksAndKeepsRealState(t *testing.T) {
	s := setup(t)
	_ = s.Heartbeat("d1", Combo{"kernel": 1, "engine": 1, "ui": 2}, true)
	_ = s.Heartbeat("d2", Combo{"kernel": 1, "engine": 1, "ui": 2}, true)
	target := Combo{"kernel": 2, "engine": 2, "ui": 3}
	camp, _ := s.StartCampaign("rollout", target, []string{"d1", "d2"}, "")
	// d1 部分应用，d2 未开始
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "kernel", Kind: CompApplied})
	if err := s.WithdrawVersion("ui", 3, true); err != nil {
		t.Fatal(err)
	}
	devices, _ := s.Status()
	if devices["d2"].Blocked == nil {
		t.Fatal("未开始设备应重新核对并给出阻碍原因")
	}
	d1 := devices["d1"]
	if d1.Components["kernel"] != CompApplied {
		t.Fatal("部分应用设备必须保留真实状态")
	}
	if len(d1.WaitingFor) == 0 {
		t.Fatal("部分应用设备应明确等待哪项收尾")
	}
}

func TestRollbackMustBeCompatibleCombo(t *testing.T) {
	s := setup(t)
	_ = s.Heartbeat("d1", Combo{"kernel": 2, "engine": 2, "ui": 3}, true)
	bad := Combo{"kernel": 1, "engine": 2, "ui": 2}
	if _, err := s.StartCampaign("rollback", bad, []string{"d1"}, ""); err == nil {
		t.Fatal("回退到不兼容组合应被拒绝")
	}
	good := Combo{"kernel": 1, "engine": 1, "ui": 2}
	camp, err := s.StartCampaign("rollback", good, []string{"d1"}, "")
	if err != nil {
		t.Fatalf("回退到仍兼容组合应被允许: %v", err)
	}
	if camp.Kind != "rollback" {
		t.Fatal("回退应记录为 rollback 战役")
	}
}

func TestLateAckRejected(t *testing.T) {
	s := setup(t)
	_ = s.Heartbeat("d1", Combo{"kernel": 1, "engine": 1, "ui": 2}, true)
	camp, _ := s.StartCampaign("rollout", Combo{"kernel": 2, "engine": 2, "ui": 3}, []string{"d1"}, "")
	if err := s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID + 99, Component: "ui", Kind: CompApplied}); err == nil {
		t.Fatal("过期战役的迟到确认应被拒绝")
	}
}

func TestOfflineDeviceBlockedInPreview(t *testing.T) {
	s := setup(t)
	_ = s.Heartbeat("d1", Combo{"kernel": 1, "engine": 1, "ui": 2}, false)
	views, _ := s.Preview(Combo{"kernel": 2, "engine": 2, "ui": 3}, []string{"d1"})
	if views[0].Feasible {
		t.Fatal("离线设备不应可行")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.RegisterVersion("kernel", 1)
	_ = s.SetRule(Rule{ID: "r", Component: "kernel", Version: 1, Requires: map[string][]int{}})
	_ = s.Heartbeat("d1", Combo{"kernel": 1}, true)
	if _, err := s.StartCampaign("rollout", Combo{"kernel": 1}, []string{"d1"}, ""); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	devices, _ := s2.Status()
	if devices["d1"].CampaignID != 1 {
		t.Fatal("重启后应能继续核对组合状态")
	}
	if len(s2.Decisions()) == 0 || len(s2.RuleEvents()) == 0 {
		t.Fatal("重启后两条历史都应保留")
	}
}

func TestNewCampaignInheritsAppliedComponents(t *testing.T) {
	s := setup(t)
	_ = s.Heartbeat("d1", Combo{"kernel": 1, "engine": 1, "ui": 2}, true)
	camp, _ := s.StartCampaign("rollout", Combo{"kernel": 2, "engine": 2, "ui": 3}, []string{"d1"}, "")
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "kernel", Kind: CompApplied})
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp.ID, Component: "engine", Kind: CompApplied})
	_ = s.WithdrawVersion("ui", 3, true)
	_ = s.RegisterVersion("ui", 4)
	_ = s.SetRule(Rule{ID: "r3", Component: "ui", Version: 4, Requires: map[string][]int{"engine": {2}}})
	camp2, err := s.StartCampaign("rollout", Combo{"kernel": 2, "engine": 2, "ui": 4}, []string{"d1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	devices, _ := s.Status()
	d := devices["d1"]
	if d.Components["kernel"] != CompApplied || d.Components["engine"] != CompApplied {
		t.Fatal("新战役应继承设备上已 applied 的组件版本")
	}
	_ = s.Ack(AckRequest{DeviceID: "d1", CampaignID: camp2.ID, Component: "ui", Kind: CompApplied})
	devices, _ = s.Status()
	if !devices["d1"].Complete || devices["d1"].Current["ui"] != 4 {
		t.Fatal("补齐剩余组件后组合应整体生效")
	}
}
