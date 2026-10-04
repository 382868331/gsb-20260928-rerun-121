package combo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type persistedData struct {
	Components []ComponentVersion     `json:"components"`
	Rulesets   []Ruleset              `json:"rulesets"`
	Campaigns  []Campaign             `json:"campaigns"`
	Plans      map[string]*DevicePlan `json:"plans"`
	Devices    map[string]*Device     `json:"devices"`
	Decisions  []Decision             `json:"decisions"`
}

// Store 持久化组件版本、约束规则代次、发布决定与设备真实状态。
type Store struct {
	mu         sync.Mutex
	dataDir    string
	components []ComponentVersion
	rulesets   []Ruleset
	campaigns  []Campaign
	plans      map[string]*DevicePlan // deviceID -> 当前行动中的计划
	devices    map[string]*Device
	decisions  []Decision
}

func NewStore(dataDir string) (*Store, error) {
	s := &Store{
		dataDir: dataDir,
		plans:   map[string]*DevicePlan{},
		devices: map[string]*Device{},
		rulesets: []Ruleset{{
			Generation: 1,
			ChangedAt:  time.Now().UTC(),
			Reason:     "初始空规则",
		}},
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) filePath() string { return filepath.Join(s.dataDir, "combo-state.json") }

func (s *Store) load() error {
	b, err := os.ReadFile(s.filePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persistedData
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("解析持久化状态失败: %w", err)
	}
	s.components = p.Components
	if len(p.Rulesets) > 0 {
		s.rulesets = p.Rulesets
	}
	s.campaigns = p.Campaigns
	if p.Plans != nil {
		s.plans = p.Plans
	}
	if p.Devices != nil {
		s.devices = p.Devices
	}
	s.decisions = p.Decisions
	return nil
}

func (s *Store) persistLocked() error {
	p := persistedData{
		Components: s.components,
		Rulesets:   s.rulesets,
		Campaigns:  s.campaigns,
		Plans:      s.plans,
		Devices:    s.devices,
		Decisions:  s.decisions,
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.filePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath())
}

// ---- 组件版本 ----

// PublishVersion 发布某组件的一个新版本。
func (s *Store) PublishVersion(component string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if component == "" || version <= 0 {
		return fmt.Errorf("组件名与正整数版本号必填")
	}
	for _, cv := range s.components {
		if cv.Component == component && cv.Version == version {
			return fmt.Errorf("%s v%d 已发布过", component, version)
		}
	}
	s.components = append(s.components, ComponentVersion{
		Component:  component,
		Version:    version,
		ReleasedAt: time.Now().UTC(),
	})
	return s.persistLocked()
}

// WithdrawVersion 撤回一个组件版本。已完成设备保留真实状态；
// 未开始设备按新情况重新核对；部分应用设备继续等待收尾项。
func (s *Store) WithdrawVersion(component string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.components {
		if s.components[i].Component == component && s.components[i].Version == version {
			now := time.Now().UTC()
			s.components[i].Withdrawn = true
			s.components[i].WithdrawnAt = &now
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%s v%d 不存在", component, version)
	}
	s.reevaluateLocked()
	return s.persistLocked()
}

// ---- 约束规则 ----

// SetRules 以新一代约束替换当前规则，旧代次保留在历史中。
func (s *Store) SetRules(rules []Rule, reason string) Ruleset {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.setRulesLocked(rules, reason)
	_ = s.persistLocked()
	return rs
}

func (s *Store) setRulesLocked(rules []Rule, reason string) Ruleset {
	rs := Ruleset{
		Generation: s.rulesets[len(s.rulesets)-1].Generation + 1,
		Rules:      append([]Rule(nil), rules...),
		Reason:     reason,
		ChangedAt:  time.Now().UTC(),
	}
	s.rulesets = append(s.rulesets, rs)
	s.reevaluateLocked()
	return rs
}

// RulesetHistory 返回全部规则代次（含当前），与发布决定历史分开查询。
func (s *Store) RulesetHistory() []Ruleset {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ruleset(nil), s.rulesets...)
}

// Decisions 返回发布/回退决定历史（只追加）。
func (s *Store) Decisions() []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Decision(nil), s.decisions...)
}

// checkLocked 核对组合在当前规则与版本清单下是否兼容，返回全部阻碍原因。
func (s *Store) checkLocked(target Combination) []string {
	var reasons []string
	keys := make([]string, 0, len(target))
	for k := range target {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, comp := range keys {
		ver := target[comp]
		found := false
		for _, cv := range s.components {
			if cv.Component == comp && cv.Version == ver {
				found = true
				if cv.Withdrawn {
					reasons = append(reasons, fmt.Sprintf("%s v%d 已被撤回", comp, ver))
				}
			}
		}
		if !found {
			reasons = append(reasons, fmt.Sprintf("%s v%d 不是已发布版本", comp, ver))
		}
	}
	current := s.rulesets[len(s.rulesets)-1]
	for _, r := range current.Rules {
		if r.violatedBy(target) {
			reasons = append(reasons, fmt.Sprintf("违反约束 %s: %s>=%d 要求 %s>=%d",
				r.ID, r.When.Component, r.When.MinVersion, r.Require.Component, r.Require.MinVersion))
		}
	}
	return reasons
}

// reevaluateLocked 在规则变化或版本撤回后重新核对当前行动：
// 已完成设备保留真实状态；未开始设备重新核对并记录阻碍原因；
// 部分应用设备不清除进度，由 WaitingOn 明确等待的收尾组件。
func (s *Store) reevaluateLocked() {
	camp := s.activeCampaignLocked()
	if camp == nil {
		return
	}
	for _, id := range camp.DeviceIDs {
		p := s.plans[id]
		if p == nil || p.Done {
			continue
		}
		if p.Started() {
			continue // 部分应用：保留进度，等待收尾项
		}
		p.Blocked = s.checkLocked(camp.Target)
	}
}

// ---- 预演与发布 ----

// Plan 让操作者先看到每台设备能否采用目标组合以及阻碍原因。
func (s *Store) Plan(target Combination) PlanResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := PlanResponse{Target: target.Clone()}
	resp.Problems = s.checkLocked(target)
	resp.Compatible = len(resp.Problems) == 0
	ids := s.sortedDeviceIDsLocked()
	for _, id := range ids {
		d := s.devices[id]
		entry := PlanEntry{DeviceID: id, Current: d.Current.Clone()}
		switch {
		case !resp.Compatible:
			entry.Reasons = resp.Problems
		case d.Current.Equal(target):
			entry.OK = true
			entry.Note = "已处于目标组合，无需推进"
		default:
			entry.OK = true
		}
		resp.Devices = append(resp.Devices, entry)
	}
	return resp
}

// Release 选择一批设备尝试推进到目标组合（整组，不是逐组件独立推进）。
func (s *Store) Release(req ReleaseRequest) (Decision, error) {
	return s.startCampaign("release", req.Target, req.DeviceIDs)
}

// Rollback 回退到一个仍然兼容的既有组合，而不是逐项恢复各自旧值。
func (s *Store) Rollback(req RollbackRequest) (Decision, error) {
	if len(req.Target) == 0 {
		return Decision{}, fmt.Errorf("回退必须指定完整的目标组合")
	}
	return s.startCampaign("rollback", req.Target, req.DeviceIDs)
}

func (s *Store) startCampaign(kind string, target Combination, deviceIDs []string) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(target) == 0 {
		return Decision{}, fmt.Errorf("目标组合不能为空")
	}
	if problems := s.checkLocked(target); len(problems) > 0 {
		return Decision{}, fmt.Errorf("目标组合不兼容: %v", problems)
	}
	if len(deviceIDs) == 0 {
		deviceIDs = s.sortedDeviceIDsLocked()
	}
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       kind,
		Target:     target.Clone(),
		RulesetGen: s.rulesets[len(s.rulesets)-1].Generation,
		At:         time.Now().UTC(),
	}
	var accepted []string
	for _, id := range deviceIDs {
		d, ok := s.devices[id]
		if !ok {
			if dec.Rejected == nil {
				dec.Rejected = map[string][]string{}
			}
			dec.Rejected[id] = []string{"设备未注册"}
			continue
		}
		// 已处于目标组合且没有未完成的在途行动时，无需推进；
		// 但若设备卡在未完成的行动中（如部分应用），回退/发布仍要带上它。
		if d.Current.Equal(target) {
			if p := s.plans[id]; p == nil || !p.Done {
				accepted = append(accepted, id)
				continue
			}
			if dec.Rejected == nil {
				dec.Rejected = map[string][]string{}
			}
			dec.Rejected[id] = []string{"已处于目标组合"}
			continue
		}
		accepted = append(accepted, id)
	}
	if len(accepted) == 0 {
		return Decision{}, fmt.Errorf("没有可推进的设备")
	}
	// 新行动取代旧行动；旧行动中未完成的设备保留真实状态，
	// 是否继续由新行动的计划决定。
	for i := range s.campaigns {
		s.campaigns[i].Active = false
	}
	camp := Campaign{
		ID:         len(s.campaigns) + 1,
		Kind:       kind,
		Target:     target.Clone(),
		DeviceIDs:  append([]string(nil), accepted...),
		RulesetGen: dec.RulesetGen,
		CreatedAt:  dec.At,
		Active:     true,
	}
	s.campaigns = append(s.campaigns, camp)
	for _, id := range accepted {
		components := map[string]*CompProgress{}
		for comp := range target {
			components[comp] = &CompProgress{State: CompPending}
		}
		s.plans[id] = &DevicePlan{DeviceID: id, Components: components}
	}
	dec.CampaignID = camp.ID
	dec.Accepted = accepted
	s.decisions = append(s.decisions, dec)
	if err := s.persistLocked(); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

// ---- 设备侧 ----

// Register 设备心跳：上报当前实际生效组合，返回应推进的目标（若在本次行动中）。
func (s *Store) Register(req RegisterRequest) (RegisterResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.DeviceID == "" {
		return RegisterResponse{}, fmt.Errorf("device_id 必填")
	}
	now := time.Now().UTC()
	d := s.devices[req.DeviceID]
	if d == nil {
		d = &Device{ID: req.DeviceID}
		s.devices[req.DeviceID] = d
	}
	d.Current = req.Current.Clone()
	d.LastSeen = &now

	camp := s.activeCampaignLocked()
	if camp == nil {
		return RegisterResponse{}, s.persistLocked()
	}
	p := s.plans[req.DeviceID]
	if p == nil {
		return RegisterResponse{}, s.persistLocked()
	}
	if d.Current.Equal(camp.Target) {
		// 心跳对账：实际组合已等于目标，视为完成（重启恢复路径）。
		if !p.Done {
			p.Done = true
			p.CompletedAt = &now
			for _, cp := range p.Components {
				cp.State = CompApplied
			}
		}
		return RegisterResponse{}, s.persistLocked()
	}
	resp := RegisterResponse{CampaignID: camp.ID, Blocked: p.Blocked}
	if len(p.Blocked) == 0 {
		t := camp.Target.Clone()
		resp.Target = &t
	}
	return resp, s.persistLocked()
}

// Ack 组件级确认。只有全部组件都 applied，整组组合才算可用；
// 迟到确认（行动 ID 不匹配）直接拒绝。
func (s *Store) Ack(req AckRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.activeCampaignLocked()
	if camp == nil {
		return fmt.Errorf("当前没有进行中的行动，确认已迟到")
	}
	if req.CampaignID != camp.ID {
		return fmt.Errorf("迟到确认：行动 %d 已结束，当前行动为 %d", req.CampaignID, camp.ID)
	}
	p := s.plans[req.DeviceID]
	if p == nil {
		return fmt.Errorf("设备 %s 不在当前行动中", req.DeviceID)
	}
	if p.Done {
		return fmt.Errorf("设备 %s 已完成当前行动", req.DeviceID)
	}
	if len(p.Blocked) > 0 {
		return fmt.Errorf("设备 %s 被阻碍: %v", req.DeviceID, p.Blocked)
	}
	cp, ok := p.Components[req.Component]
	if !ok {
		return fmt.Errorf("组件 %s 不在目标组合中", req.Component)
	}
	switch req.Kind {
	case AckDownloaded:
		if cp.State == CompPending {
			cp.State = CompDownloaded
		}
	case AckValidated:
		if cp.State == CompPending || cp.State == CompDownloaded {
			cp.State = CompValidated
		}
	case AckApplied:
		cp.State = CompApplied
		cp.Error = ""
	case AckFailed:
		if cp.State != CompApplied {
			cp.State = CompFailed
			cp.Error = req.Reason
		}
	default:
		return fmt.Errorf("未知确认类型 %q", req.Kind)
	}
	// 整组判定：所有组件都 applied 才认为组合可用。
	allApplied := true
	for _, c := range p.Components {
		if c.State != CompApplied {
			allApplied = false
			break
		}
	}
	if allApplied {
		now := time.Now().UTC()
		p.Done = true
		p.CompletedAt = &now
		if d := s.devices[req.DeviceID]; d != nil {
			d.Current = camp.Target.Clone()
		}
	}
	return s.persistLocked()
}

// ---- 状态视图 ----

func (s *Store) Snapshot(online func(id string) bool) StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := StatusResponse{
		Components: append([]ComponentVersion(nil), s.components...),
		RulesetGen: s.rulesets[len(s.rulesets)-1].Generation,
		Rules:      append([]Rule(nil), s.rulesets[len(s.rulesets)-1].Rules...),
	}
	camp := s.activeCampaignLocked()
	if camp != nil {
		c := *camp
		resp.Campaign = &c
	}
	for _, id := range s.sortedDeviceIDsLocked() {
		d := s.devices[id]
		v := DeviceView{
			DeviceID: id,
			Current:  d.Current.Clone(),
			LastSeen: d.LastSeen,
		}
		if online != nil {
			v.Online = online(id)
		}
		if camp != nil {
			if p := s.plans[id]; p != nil {
				v.InCampaign = true
				v.Target = camp.Target.Clone()
				v.Components = p.Components
				v.Blocked = p.Blocked
				v.Done = p.Done
				if !p.Done && len(p.Blocked) == 0 {
					v.WaitingOn = p.WaitingOn()
				}
			}
		}
		resp.Devices = append(resp.Devices, v)
	}
	return resp
}

func (s *Store) activeCampaignLocked() *Campaign {
	for i := range s.campaigns {
		if s.campaigns[i].Active {
			return &s.campaigns[i]
		}
	}
	return nil
}

func (s *Store) sortedDeviceIDsLocked() []string {
	ids := make([]string, 0, len(s.devices))
	for id := range s.devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
