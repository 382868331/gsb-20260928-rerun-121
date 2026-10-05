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
// 只核对当前批次：后续批次仍待推进，其阻碍原因在进入该批时才核对。
func (s *Store) reevaluateLocked() {
	camp := s.activeCampaignLocked()
	if camp == nil || camp.ActiveBatch < 0 || camp.ActiveBatch >= len(camp.Batches) {
		return
	}
	for _, id := range camp.Batches[camp.ActiveBatch].DeviceIDs {
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
// 指定 batches 时按批推进：第一批是试运行组，达到该批确认条件后
// 由操作者显式 advance 才进入下一批。
func (s *Store) Release(req ReleaseRequest) (Decision, error) {
	batches := req.Batches
	if len(batches) == 0 {
		batches = []BatchRequest{{DeviceIDs: req.DeviceIDs, Condition: BatchCondition{Kind: "all"}}}
	}
	return s.startCampaign("release", req.Target, batches)
}

// Rollback 回退到一个仍然兼容的既有组合，而不是逐项恢复各自旧值。
// FailedOnly 时只回退当前行动中真实失败的设备；已经采用新组合的
// 其它设备不在回退范围内，可继续按新组合服务。
func (s *Store) Rollback(req RollbackRequest) (Decision, error) {
	if len(req.Target) == 0 {
		return Decision{}, fmt.Errorf("回退必须指定完整的目标组合")
	}
	deviceIDs := req.DeviceIDs
	if req.FailedOnly {
		failed, err := s.failedDeviceIDs()
		if err != nil {
			return Decision{}, err
		}
		deviceIDs = failed
	}
	return s.startCampaign("rollback", req.Target,
		[]BatchRequest{{DeviceIDs: deviceIDs, Condition: BatchCondition{Kind: "all"}}})
}

// failedDeviceIDs 返回当前行动中存在组件失败的设备（按 ID 排序）。
func (s *Store) failedDeviceIDs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.activeCampaignLocked()
	if camp == nil {
		return nil, fmt.Errorf("当前没有进行中的行动，无法只回退失败设备")
	}
	var out []string
	for _, id := range camp.DeviceIDs {
		p := s.plans[id]
		if p == nil || p.Done {
			continue
		}
		for _, cp := range p.Components {
			if cp.State == CompFailed {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("当前行动中没有失败的设备")
	}
	return out, nil
}

func (s *Store) startCampaign(kind string, target Combination, batchReqs []BatchRequest) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(target) == 0 {
		return Decision{}, fmt.Errorf("目标组合不能为空")
	}
	if problems := s.checkLocked(target); len(problems) > 0 {
		return Decision{}, fmt.Errorf("目标组合不兼容: %v", problems)
	}
	if len(batchReqs) == 0 {
		return Decision{}, fmt.Errorf("至少需要一个批次")
	}
	// 汇总批次设备，跨批去重；未指定任何设备时默认全部已注册设备为一批。
	seen := map[string]bool{}
	var batchDeviceIDs [][]string
	for i, br := range batchReqs {
		ids := br.DeviceIDs
		if len(batchReqs) == 1 && len(ids) == 0 {
			ids = s.sortedDeviceIDsLocked()
		}
		if len(ids) == 0 {
			return Decision{}, fmt.Errorf("批次 %d 没有设备", i)
		}
		var dedup []string
		for _, id := range ids {
			if seen[id] {
				return Decision{}, fmt.Errorf("设备 %s 出现在多个批次中", id)
			}
			seen[id] = true
			dedup = append(dedup, id)
		}
		batchDeviceIDs = append(batchDeviceIDs, dedup)
	}
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       kind,
		Target:     target.Clone(),
		RulesetGen: s.rulesets[len(s.rulesets)-1].Generation,
		At:         time.Now().UTC(),
	}
	var acceptedBatches [][]string
	var acceptedAll []string
	for _, ids := range batchDeviceIDs {
		var accepted []string
		for _, id := range ids {
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
		acceptedBatches = append(acceptedBatches, accepted)
		acceptedAll = append(acceptedAll, accepted...)
	}
	if len(acceptedAll) == 0 {
		return Decision{}, fmt.Errorf("没有可推进的设备")
	}
	// 新行动取代旧行动；旧行动中未完成的设备保留真实状态，
	// 是否继续由新行动的计划决定。
	for i := range s.campaigns {
		s.campaigns[i].Active = false
	}
	now := dec.At
	batches := make([]Batch, 0, len(batchReqs))
	for i, br := range batchReqs {
		b := Batch{
			Index:     i,
			DeviceIDs: append([]string(nil), acceptedBatches[i]...),
			Condition: br.Condition,
			State:     BatchWaiting,
		}
		if b.Condition.Kind == "" {
			b.Condition.Kind = "all"
		}
		if i == 0 {
			b.State = BatchActive
			b.StartedAt = &now
		}
		batches = append(batches, b)
	}
	camp := Campaign{
		ID:          len(s.campaigns) + 1,
		Kind:        kind,
		Target:      target.Clone(),
		DeviceIDs:   append([]string(nil), acceptedAll...),
		RulesetGen:  dec.RulesetGen,
		CreatedAt:   dec.At,
		Active:      true,
		Batches:     batches,
		ActiveBatch: 0,
	}
	s.campaigns = append(s.campaigns, camp)
	for batchIdx, ids := range acceptedBatches {
		for _, id := range ids {
			d := s.devices[id]
			components := map[string]*CompProgress{}
			allApplied := true
			for comp, ver := range target {
				// 新行动不擦掉已有事实：设备上已真实生效的版本
				// 直接记为 applied，只等待确实需要变化的组件。
				if d != nil && d.Current[comp] == ver {
					components[comp] = &CompProgress{State: CompApplied}
					continue
				}
				components[comp] = &CompProgress{State: CompPending}
				allApplied = false
			}
			p := &DevicePlan{DeviceID: id, Batch: batchIdx, Components: components}
			if allApplied {
				completedAt := dec.At
				p.Done = true
				p.CompletedAt = &completedAt
			}
			s.plans[id] = p
		}
	}
	dec.CampaignID = camp.ID
	dec.Accepted = acceptedAll
	dec.Batches = append([]Batch(nil), camp.Batches...)
	s.decisions = append(s.decisions, dec)
	s.reevaluateLocked()
	if err := s.persistLocked(); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

// batchConditionMetLocked 判定某批的组件确认条件是否已满足。
// 只统计本批设备的真实完成数，不用其它批次的状态代替。
func (s *Store) batchConditionMetLocked(b *Batch) bool {
	done := 0
	for _, id := range b.DeviceIDs {
		if p := s.plans[id]; p != nil && p.Done {
			done++
		}
	}
	if b.Condition.Kind == "count" {
		return done >= b.Condition.Count
	}
	return done == len(b.DeviceIDs)
}

// AdvanceBatch 在当前批达到确认条件后，由操作者显式确认推进到下一批。
// 暂停推进期间拒绝；进入新批次时按最新规则/版本清单重新核对未开始设备。
func (s *Store) AdvanceBatch() (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.activeCampaignLocked()
	if camp == nil {
		return Decision{}, fmt.Errorf("当前没有进行中的行动")
	}
	if camp.Paused {
		return Decision{}, fmt.Errorf("推进已暂停，请先恢复")
	}
	if camp.ActiveBatch < 0 || camp.ActiveBatch >= len(camp.Batches) {
		return Decision{}, fmt.Errorf("当前行动没有待完成的批次")
	}
	cur := &camp.Batches[camp.ActiveBatch]
	if !s.batchConditionMetLocked(cur) {
		return Decision{}, fmt.Errorf("批次 %d 尚未满足确认条件（%s），不能推进", cur.Index, cur.Condition.String())
	}
	now := time.Now().UTC()
	cur.State = BatchDone
	cur.FinishedAt = &now
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       "advance",
		CampaignID: camp.ID,
		Batch:      cur.Index,
		Target:     camp.Target.Clone(),
		RulesetGen: s.rulesets[len(s.rulesets)-1].Generation,
		At:         now,
	}
	next := camp.ActiveBatch + 1
	if next >= len(camp.Batches) {
		camp.Active = false
		camp.ActiveBatch = -1
		dec.Note = "全部批次完成"
	} else {
		camp.ActiveBatch = next
		nb := &camp.Batches[next]
		nb.State = BatchActive
		nb.StartedAt = &now
		dec.Note = fmt.Sprintf("推进到批次 %d", next)
		// 新批次按当前规则与版本清单重新核对未开始设备。
		for _, id := range nb.DeviceIDs {
			if p := s.plans[id]; p != nil && !p.Done && !p.Started() {
				p.Blocked = s.checkLocked(camp.Target)
			}
		}
	}
	s.decisions = append(s.decisions, dec)
	if err := s.persistLocked(); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

// SetPaused 暂停或恢复推进。暂停期间当前批设备可继续收尾，
// 但不允许确认进入下一批；决定与状态持久化，重启后保留。
func (s *Store) SetPaused(paused bool) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	camp := s.activeCampaignLocked()
	if camp == nil {
		return Decision{}, fmt.Errorf("当前没有进行中的行动")
	}
	if camp.Paused == paused {
		return Decision{}, fmt.Errorf("推进状态未变化")
	}
	camp.Paused = paused
	kind := "pause"
	if !paused {
		kind = "resume"
	}
	dec := Decision{
		Seq:        len(s.decisions) + 1,
		Kind:       kind,
		CampaignID: camp.ID,
		Batch:      camp.ActiveBatch,
		Target:     camp.Target.Clone(),
		RulesetGen: s.rulesets[len(s.rulesets)-1].Generation,
		At:         time.Now().UTC(),
	}
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
	// 只有当前批次中的设备会收到目标；后续批次保持等待，
	// 上一批的完成不代表本批可以开始。
	if p.Batch != camp.ActiveBatch {
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
	// 迟到旧行动/旧批次的确认不得越过当前批次：设备所属批次
	// 必须是当前推进中的批次，否则视为迟到确认直接拒绝。
	if p.Batch != camp.ActiveBatch {
		return fmt.Errorf("迟到确认：设备 %s 属于批次 %d，当前推进批次为 %d",
			req.DeviceID, p.Batch, camp.ActiveBatch)
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
		resp.Paused = c.Paused
	}
	for _, id := range s.sortedDeviceIDsLocked() {
		v := s.deviceViewLocked(id, camp, online)
		resp.Devices = append(resp.Devices, v)
	}
	// 各批次独立展示：本批设备的实际组合、等待组件与失败原因
	// 只来自本批计划，不用上一批的完成状态代表后续批次。
	if camp != nil {
		for i := range camp.Batches {
			b := &camp.Batches[i]
			bv := BatchView{
				Index:        b.Index,
				State:        b.State,
				Condition:    b.Condition,
				ConditionMet: b.State == BatchDone || s.batchConditionMetLocked(b),
			}
			for _, id := range b.DeviceIDs {
				bv.Devices = append(bv.Devices, s.deviceViewLocked(id, camp, online))
			}
			resp.Batches = append(resp.Batches, bv)
		}
	}
	return resp
}

func (s *Store) deviceViewLocked(id string, camp *Campaign, online func(string) bool) DeviceView {
	d := s.devices[id]
	v := DeviceView{
		DeviceID: id,
		Current:  d.Current.Clone(),
		LastSeen: d.LastSeen,
	}
	if online != nil {
		v.Online = online(id)
	}
	if camp == nil {
		return v
	}
	p := s.plans[id]
	if p == nil {
		return v
	}
	v.InCampaign = true
	v.Batch = p.Batch
	v.Target = camp.Target.Clone()
	v.Components = p.Components
	v.Blocked = p.Blocked
	v.Done = p.Done
	if !p.Done && len(p.Blocked) == 0 {
		v.WaitingOn = p.WaitingOn()
	}
	names := make([]string, 0, len(p.Components))
	for name := range p.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cp := p.Components[name]
		if cp.State == CompFailed {
			msg := name + " 应用失败"
			if cp.Error != "" {
				msg += ": " + cp.Error
			}
			v.Failed = append(v.Failed, msg)
		}
	}
	return v
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
