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
	for _, p := range s.plans {
		if p.CampaignID == 0 {
			for _, camp := range s.campaigns {
				for _, id := range camp.DeviceIDs {
					if id == p.DeviceID {
						p.CampaignID = camp.ID
						break
					}
				}
				if p.CampaignID != 0 {
					break
				}
			}
		}
	}
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
	for _, camp := range s.campaigns {
		if !camp.Active {
			continue
		}
		ids := camp.DeviceIDs
		for _, batch := range camp.Batches {
			if batch.Status == BatchActive {
				ids = batch.DeviceIDs
			}
		}
		for _, id := range ids {
			p := s.findPlanLocked(camp.ID, id, camp.CurrentBatchNo)
			if p == nil || p.Done || p.RolledBack {
				continue
			}
			if p.Started() {
				continue // 部分应用：保留进度，等待收尾项
			}
			p.Blocked = s.checkLocked(camp.Target)
		}
	}
}

func planKey(campaignID int, deviceID string, batchNo int) string {
	return fmt.Sprintf("%d:%04d:%s", campaignID, batchNo, deviceID)
}

func (s *Store) planForCampaignLocked(camp *Campaign, deviceID string) *DevicePlan {
	if camp == nil {
		return nil
	}
	if len(camp.Batches) > 0 {
		return s.findPlanLocked(camp.ID, deviceID, camp.CurrentBatchNo)
	}
	if p := s.plans[planKey(camp.ID, deviceID, 0)]; p != nil {
		return p
	}
	return s.plans[deviceID]
}

func (s *Store) findPlanLocked(campaignID int, deviceID string, batchNo int) *DevicePlan {
	if p := s.plans[planKey(campaignID, deviceID, batchNo)]; p != nil {
		return p
	}
	if batchNo > 0 {
		return s.plans[planKey(campaignID, deviceID, 0)]
	}
	return nil
}

func (s *Store) activePlanForDeviceLocked(deviceID string) *DevicePlan {
	for i := len(s.campaigns) - 1; i >= 0; i-- {
		camp := s.campaigns[i]
		if !camp.Active {
			continue
		}
		if p := s.planForCampaignLocked(&camp, deviceID); p != nil {
			return p
		}
	}
	return nil
}

func (s *Store) latestPlanForDeviceLocked(deviceID string) *DevicePlan {
	for i := len(s.campaigns) - 1; i >= 0; i-- {
		camp := s.campaigns[i]
		if p := s.planForCampaignLocked(&camp, deviceID); p != nil {
			return p
		}
	}
	return nil
}

func (s *Store) makePlanLocked(camp *Campaign, id string, batchNo int) *DevicePlan {
	d := s.devices[id]
	components := map[string]*CompProgress{}
	allApplied := true
	for comp, ver := range camp.Target {
		if d != nil && d.Current[comp] == ver {
			components[comp] = &CompProgress{State: CompApplied}
			continue
		}
		components[comp] = &CompProgress{State: CompPending}
		allApplied = false
	}
	p := &DevicePlan{CampaignID: camp.ID, BatchNo: batchNo, DeviceID: id, Components: components}
	if allApplied {
		completedAt := camp.CreatedAt
		p.Done = true
		p.CompletedAt = &completedAt
	}
	s.plans[planKey(camp.ID, id, batchNo)] = p
	return p
}

func failedComponents(p *DevicePlan) []string {
	if p == nil {
		return nil
	}
	var out []string
	for name, cp := range p.Components {
		if cp.State == CompFailed {
			if cp.Error != "" {
				out = append(out, name+": "+cp.Error)
			} else {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) batchReadyLocked(camp *Campaign, batch BatchState) (bool, []string) {
	required := batch.RequiredComponents
	if len(required) == 0 {
		required = componentNames(camp.Target)
	}
	var problems []string
	for _, id := range batch.DeviceIDs {
		p := s.findPlanLocked(camp.ID, id, batch.No)
		if p == nil {
			problems = append(problems, id+": 缺少确认记录")
			continue
		}
		for _, comp := range required {
			cp := p.Components[comp]
			if cp == nil {
				problems = append(problems, id+": 缺少组件 "+comp)
				continue
			}
			if cp.State != CompApplied {
				problems = append(problems, id+": "+comp+" 状态为 "+cp.State)
			}
		}
	}
	return len(problems) == 0, problems
}

func componentNames(target Combination) []string {
	out := make([]string, 0, len(target))
	for comp := range target {
		out = append(out, comp)
	}
	sort.Strings(out)
	return out
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req.Target) == 0 {
		return Decision{}, fmt.Errorf("目标组合不能为空")
	}
	if problems := s.checkLocked(req.Target); len(problems) > 0 {
		return Decision{}, fmt.Errorf("目标组合不兼容: %v", problems)
	}
	if len(req.Batches) > 0 {
		return s.startStagedReleaseLocked(req)
	}
	return s.startSimpleCampaignLocked("release", req.Target, req.DeviceIDs)
}

// Rollback 回退到一个仍然兼容的既有组合，而不是逐项恢复各自旧值。
func (s *Store) Rollback(req RollbackRequest) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req.Target) == 0 {
		return Decision{}, fmt.Errorf("回退必须指定完整的目标组合")
	}
	if problems := s.checkLocked(req.Target); len(problems) > 0 {
		return Decision{}, fmt.Errorf("目标组合不兼容: %v", problems)
	}
	if req.OnlyFailed {
		return s.rollbackFailedLocked(req)
	}
	return s.startSimpleCampaignLocked("rollback", req.Target, req.DeviceIDs)
}

func (s *Store) startSimpleCampaignLocked(kind string, target Combination, deviceIDs []string) (Decision, error) {
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
	var parent *Campaign
	for i := range s.campaigns {
		c := &s.campaigns[i]
		if c.Active && (kind == "rollback" || c.Kind == "release") {
			parent = c
			c.Active = false
			c.Status = CampaignCompleted
			break
		}
	}
	camp := Campaign{
		ID:         len(s.campaigns) + 1,
		Kind:       kind,
		Target:     target.Clone(),
		DeviceIDs:  append([]string(nil), accepted...),
		RulesetGen: dec.RulesetGen,
		CreatedAt:  dec.At,
		Active:     true,
		Status:     CampaignActive,
	}
	if parent != nil {
		camp.ParentCampaignID = parent.ID
	}
	s.campaigns = append(s.campaigns, camp)
	for _, id := range accepted {
		s.makePlanLocked(&camp, id, 0)
	}
	dec.CampaignID = camp.ID
	dec.Accepted = accepted
	s.decisions = append(s.decisions, dec)
	if err := s.persistLocked(); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

func (s *Store) startStagedReleaseLocked(req ReleaseRequest) (Decision, error) {
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       "release",
		Target:     req.Target.Clone(),
		RulesetGen: s.rulesets[len(s.rulesets)-1].Generation,
		At:         time.Now().UTC(),
		Batches:    append([]BatchSpec(nil), req.Batches...),
	}
	seen := map[string]bool{}
	camp := Campaign{
		ID:             len(s.campaigns) + 1,
		Kind:           "release",
		Target:         req.Target.Clone(),
		RulesetGen:     dec.RulesetGen,
		CreatedAt:      dec.At,
		Active:         true,
		Status:         CampaignActive,
		CurrentBatchNo: 1,
	}
	for i, spec := range req.Batches {
		no := i + 1
		batch := BatchState{No: no, Name: spec.Name, RequiredComponents: append([]string(nil), spec.RequiredComponents...), Status: BatchPending}
		for _, id := range spec.DeviceIDs {
			if seen[id] {
				if dec.Rejected == nil {
					dec.Rejected = map[string][]string{}
				}
				dec.Rejected[id] = []string{"设备重复出现在多个批次"}
				continue
			}
			seen[id] = true
			if _, ok := s.devices[id]; !ok {
				if dec.Rejected == nil {
					dec.Rejected = map[string][]string{}
				}
				dec.Rejected[id] = []string{"设备未注册"}
				continue
			}
			batch.DeviceIDs = append(batch.DeviceIDs, id)
			batch.OriginalDeviceIDs = append(batch.OriginalDeviceIDs, id)
			camp.DeviceIDs = append(camp.DeviceIDs, id)
			if no == 1 {
				dec.Accepted = append(dec.Accepted, id)
			}
		}
		camp.Batches = append(camp.Batches, batch)
	}
	if len(camp.Batches) == 0 || len(camp.Batches[0].DeviceIDs) == 0 {
		return Decision{}, fmt.Errorf("首批至少需要一台已注册设备")
	}
	for i := range s.campaigns {
		if s.campaigns[i].Active && s.campaigns[i].Kind == "release" {
			s.campaigns[i].Active = false
			s.campaigns[i].Status = CampaignCompleted
		}
	}
	camp.Batches[0].Status = BatchActive
	for _, id := range camp.Batches[0].DeviceIDs {
		s.makePlanLocked(&camp, id, 1)
	}
	s.campaigns = append(s.campaigns, camp)
	dec.CampaignID = camp.ID
	s.decisions = append(s.decisions, dec)
	if err := s.persistLocked(); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

func (s *Store) PauseCampaign(id int) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.campaignByIDLocked(id)
	if camp == nil || !camp.Active || camp.Kind != "release" || len(camp.Batches) == 0 {
		return Decision{}, fmt.Errorf("没有可暂停的分批发布行动")
	}
	camp.Paused = true
	dec := s.addDecisionLocked("batch_pause", camp, nil, camp.CurrentBatchNo)
	return dec, s.persistLocked()
}

func (s *Store) ResumeCampaign(id int) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.campaignByIDLocked(id)
	if camp == nil || !camp.Active || camp.Kind != "release" || len(camp.Batches) == 0 {
		return Decision{}, fmt.Errorf("没有可恢复的分批发布行动")
	}
	camp.Paused = false
	dec := s.addDecisionLocked("batch_resume", camp, nil, camp.CurrentBatchNo)
	return dec, s.persistLocked()
}

func (s *Store) AdvanceBatch(req BatchAdvanceRequest) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.activeStagedCampaignLocked(req.CampaignID)
	if camp == nil {
		return Decision{}, fmt.Errorf("没有可推进的分批发布行动")
	}
	if camp.Paused {
		return Decision{}, fmt.Errorf("发布已暂停，不能推进下一批")
	}
	current := &camp.Batches[camp.CurrentBatchNo-1]
	ready, problems := s.batchReadyLocked(camp, *current)
	if !ready {
		return Decision{}, fmt.Errorf("当前批次尚未满足组件确认条件: %v", problems)
	}
	if camp.CurrentBatchNo >= len(camp.Batches) {
		return Decision{}, fmt.Errorf("已经是最后一批")
	}
	current.Status = BatchCompleted
	camp.CurrentBatchNo++
	next := &camp.Batches[camp.CurrentBatchNo-1]
	next.Status = BatchActive
	var accepted []string
	for _, id := range next.DeviceIDs {
		s.makePlanLocked(camp, id, next.No)
		accepted = append(accepted, id)
	}
	dec := s.addDecisionLocked("batch_advance", camp, accepted, next.No)
	return dec, s.persistLocked()
}

func (s *Store) rollbackFailedLocked(req RollbackRequest) (Decision, error) {
	camp := s.campaignByIDLocked(req.CampaignID)
	if camp == nil || !camp.Active || camp.Kind != "release" {
		return Decision{}, fmt.Errorf("only_failed 回退必须指定进行中的发布行动")
	}
	candidates := append([]string(nil), req.DeviceIDs...)
	if len(candidates) == 0 {
		if len(camp.Batches) > 0 {
			candidates = camp.Batches[camp.CurrentBatchNo-1].DeviceIDs
		} else {
			candidates = camp.DeviceIDs
		}
	}
	var failed []string
	rejected := map[string][]string{}
	for _, id := range candidates {
		p := s.planForCampaignLocked(camp, id)
		if p == nil {
			rejected[id] = []string{"设备不在当前批次"}
			continue
		}
		if reasons := failedComponents(p); len(reasons) > 0 {
			failed = append(failed, id)
		} else {
			rejected[id] = []string{"设备没有失败组件，继续保持新组合服务"}
		}
	}
	if len(failed) == 0 {
		return Decision{}, fmt.Errorf("没有需要回退的失败设备")
	}
	rb := Campaign{
		ID:               len(s.campaigns) + 1,
		Kind:             "rollback",
		Target:           req.Target.Clone(),
		DeviceIDs:        append([]string(nil), failed...),
		RulesetGen:       s.rulesets[len(s.rulesets)-1].Generation,
		CreatedAt:        time.Now().UTC(),
		Active:           true,
		Status:           CampaignActive,
		ParentCampaignID: camp.ID,
	}
	for _, id := range failed {
		s.makePlanLocked(&rb, id, 0)
		p := s.plans[planKey(rb.ID, id, 0)]
		p.BatchNo = 0
		s.markDeviceRolledBackLocked(camp, id)
	}
	s.campaigns = append(s.campaigns, rb)
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       "rollback",
		CampaignID: rb.ID,
		Target:     req.Target.Clone(),
		RulesetGen: rb.RulesetGen,
		Accepted:   failed,
		Rejected:   rejected,
		At:         rb.CreatedAt,
	}
	s.decisions = append(s.decisions, dec)
	return dec, s.persistLocked()
}

func (s *Store) campaignByIDLocked(id int) *Campaign {
	if id == 0 {
		return s.activeCampaignLocked()
	}
	for i := range s.campaigns {
		if s.campaigns[i].ID == id {
			return &s.campaigns[i]
		}
	}
	return nil
}

func (s *Store) activeStagedCampaignLocked(id int) *Campaign {
	camp := s.campaignByIDLocked(id)
	if camp != nil && camp.Active && camp.Kind == "release" && len(camp.Batches) > 0 {
		return camp
	}
	return nil
}

func (s *Store) addDecisionLocked(kind string, camp *Campaign, accepted []string, batchNo int) Decision {
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       kind,
		CampaignID: camp.ID,
		Target:     camp.Target.Clone(),
		RulesetGen: camp.RulesetGen,
		Accepted:   append([]string(nil), accepted...),
		BatchNo:    batchNo,
		At:         time.Now().UTC(),
	}
	s.decisions = append(s.decisions, dec)
	return dec
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

	p := s.activePlanForDeviceLocked(req.DeviceID)
	if p == nil {
		return RegisterResponse{}, s.persistLocked()
	}
	camp := s.campaignByIDLocked(p.CampaignID)
	if camp == nil || !camp.Active {
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
			s.afterDeviceCompletedLocked(camp, req.DeviceID)
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
	camp := s.campaignByIDLocked(req.CampaignID)
	if camp == nil || !camp.Active {
		return fmt.Errorf("当前没有进行中的行动，确认已迟到")
	}
	p := s.planForCampaignLocked(camp, req.DeviceID)
	if p == nil {
		return fmt.Errorf("设备 %s 不在行动 %d 的当前批次中", req.DeviceID, camp.ID)
	}
	if req.BatchNo != 0 && req.BatchNo != p.BatchNo {
		return fmt.Errorf("迟到确认：行动 %d 当前批次为 %d，收到批次 %d", camp.ID, p.BatchNo, req.BatchNo)
	}
	if p.Done {
		return fmt.Errorf("设备 %s 已完成当前行动", req.DeviceID)
	}
	if len(p.Blocked) > 0 {
		return fmt.Errorf("设备 %s 被阻碍: %v", req.DeviceID, p.Blocked)
	}
	if p.RolledBack {
		return fmt.Errorf("设备 %s 已从原行动回退，迟到确认不得改变回退事实", req.DeviceID)
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
		// 组件级 applied 是事实：立即记入设备实际生效组合，
		// 与整组组合是否切换完成（Done）分别展示。
		if d := s.devices[req.DeviceID]; d != nil {
			if d.Current == nil {
				d.Current = Combination{}
			}
			d.Current[req.Component] = camp.Target[req.Component]
		}
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
		s.afterDeviceCompletedLocked(camp, req.DeviceID)
	}
	return s.persistLocked()
}

func (s *Store) completeCampaignIfReadyLocked(camp *Campaign) {
	if camp == nil || !camp.Active || (len(camp.Batches) > 0 && camp.CurrentBatchNo != len(camp.Batches)) {
		return
	}
	ids := camp.DeviceIDs
	if len(camp.Batches) > 0 && camp.CurrentBatchNo > 0 {
		ids = camp.Batches[camp.CurrentBatchNo-1].DeviceIDs
	}
	for _, id := range ids {
		p := s.findPlanLocked(camp.ID, id, camp.CurrentBatchNo)
		if p == nil || !p.Done {
			return
		}
	}
	camp.Active = false
	camp.Status = CampaignCompleted
}

func (s *Store) afterDeviceCompletedLocked(camp *Campaign, deviceID string) {
	if camp.Kind == "rollback" && camp.ParentCampaignID != 0 {
		if parent := s.campaignByIDLocked(camp.ParentCampaignID); parent != nil && parent.Active {
			s.markDeviceRolledBackLocked(parent, deviceID)
		}
	}
	s.completeCampaignIfReadyLocked(camp)
}

func (s *Store) markDeviceRolledBackLocked(parent *Campaign, deviceID string) {
	if len(parent.Batches) > 0 {
		batch := &parent.Batches[parent.CurrentBatchNo-1]
		batch.DeviceIDs = removeString(batch.DeviceIDs, deviceID)
		batch.RolledBackDeviceIDs = append(batch.RolledBackDeviceIDs, deviceID)
	}
	for batchNo := range parent.Batches {
		if p := s.findPlanLocked(parent.ID, deviceID, batchNo+1); p != nil {
			p.RolledBack = true
		}
	}
	if p := s.plans[planKey(parent.ID, deviceID, 0)]; p != nil {
		p.RolledBack = true
	}
}

func removeString(items []string, want string) []string {
	out := items[:0]
	for _, item := range items {
		if item != want {
			out = append(out, item)
		}
	}
	return out
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
	if camp == nil {
		camp = s.primaryCompletedCampaignLocked()
	}
	if camp != nil {
		c := *camp
		resp.Campaign = &c
	}
	for i := range s.campaigns {
		c := s.campaigns[i]
		resp.Campaigns = append(resp.Campaigns, c)
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
		p := s.latestPlanForDeviceLocked(id)
		if p != nil {
			pcamp := s.campaignByIDLocked(p.CampaignID)
			if pcamp != nil {
				v.InCampaign = true
				v.CampaignID = pcamp.ID
				v.BatchNo = p.BatchNo
				v.Target = pcamp.Target.Clone()
				v.Components = p.Components
				v.Blocked = p.Blocked
				v.Done = p.Done
				v.RolledBack = p.RolledBack
				v.Failed = failedComponents(p)
				if p.BatchNo > 0 {
					batch := &pcamp.Batches[p.BatchNo-1]
					v.BatchName = batch.Name
					v.BatchStatus = batch.Status
				}
				if !p.Done && len(p.Blocked) == 0 {
					v.WaitingOn = p.WaitingOn()
				}
			}
		}
		resp.Devices = append(resp.Devices, v)
	}
	if camp != nil && len(camp.Batches) > 0 {
		for i := range camp.Batches {
			batch := camp.Batches[i]
			bv := BatchView{
				No:                  batch.No,
				Name:                batch.Name,
				Status:              batch.Status,
				RequiredComponents:  append([]string(nil), batch.RequiredComponents...),
				DeviceIDs:           append([]string(nil), batch.DeviceIDs...),
				RolledBackDeviceIDs: append([]string(nil), batch.RolledBackDeviceIDs...),
			}
			if batch.Status == BatchActive && !camp.Paused {
				bv.ReadyToAdvance, _ = s.batchReadyLocked(camp, batch)
			}
			for _, id := range batch.OriginalDeviceIDs {
				d := s.devices[id]
				dv := DeviceView{DeviceID: id, Current: d.Current.Clone()}
				if online != nil {
					dv.Online = online(id)
				}
				p := s.findPlanLocked(camp.ID, id, batch.No)
				if p != nil {
					dv.CampaignID = camp.ID
					dv.BatchNo = batch.No
					dv.BatchName = batch.Name
					dv.BatchStatus = batch.Status
					dv.Target = camp.Target.Clone()
					dv.Components = p.Components
					dv.Blocked = p.Blocked
					dv.Done = p.Done
					dv.RolledBack = p.RolledBack
					dv.Failed = failedComponents(p)
					if !p.Done && len(p.Blocked) == 0 && !p.RolledBack {
						dv.WaitingOn = p.WaitingOn()
					}
				}
				bv.Devices = append(bv.Devices, dv)
			}
			resp.Batches = append(resp.Batches, bv)
		}
	}
	return resp
}

func (s *Store) activeCampaignLocked() *Campaign {
	var first *Campaign
	for i := range s.campaigns {
		if s.campaigns[i].Active {
			if first == nil {
				first = &s.campaigns[i]
			}
			if s.campaigns[i].Kind != "rollback" {
				return &s.campaigns[i]
			}
		}
	}
	return first
}

func (s *Store) primaryCompletedCampaignLocked() *Campaign {
	for i := len(s.campaigns) - 1; i >= 0; i-- {
		if s.campaigns[i].Kind != "rollback" {
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
