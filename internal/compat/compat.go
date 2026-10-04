// Package compat 实现多组件兼容组合的发布编排。
//
// 每台设备同时运行多个相互依赖的组件，只有把某个组件推进到新版本后
// 整台设备的组合仍满足兼容约束，该推进才被允许。核心语义：
//
//   - 组合（Combo）是设备上各组件版本的完整快照，兼容校验以组合为单位；
//   - 单个组件 downloaded/validated/applied 只推进该组件自身状态，
//     绝不代表整个组合可用；组合内全部组件 applied 才算切换完成；
//   - 操作者先通过 Preview 看到每台设备可行性与阻碍原因，再圈选批次；
//   - 试用期间修改约束或撤回版本：未开始设备重新核对，部分应用设备
//     保留真实状态并明确列出等待收尾的组件；
//   - 回退选择一个仍兼容的完整组合作为目标，而不是逐项恢复旧值；
//   - 发布决定与规则变化分别记入两条可独立查询的历史。
package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 组件在设备上针对目标组合的状态。
const (
	CompPending    = "pending"
	CompDownloaded = "downloaded"
	CompValidated  = "validated"
	CompApplied    = "applied"
	CompFailed     = "failed"
)

// Combo 是设备上一组组件版本的完整组合。
type Combo map[string]int

// Key 返回组合的规范化字符串，便于日志与比较。
func (c Combo) Key() string {
	parts := make([]string, 0, len(c))
	for name, v := range c {
		parts = append(parts, fmt.Sprintf("%s@%d", name, v))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ", ") + "}"
}

func (c Combo) clone() Combo {
	out := make(Combo, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// Rule 表示一条兼容约束：当设备上 Component 处于 Version 时，
// Requires 中每个依赖组件的版本必须落在允许集合内。
type Rule struct {
	ID        string           `json:"id"`
	Component string           `json:"component"`
	Version   int              `json:"version"`
	Requires  map[string][]int `json:"requires"`
	Note      string           `json:"note,omitempty"`
}

// ComponentVersion 记录一个已发布的组件版本。
type ComponentVersion struct {
	Component  string    `json:"component"`
	Version    int       `json:"version"`
	Withdrawn  bool      `json:"withdrawn"`
	ReleasedAt time.Time `json:"released_at"`
}

// Campaign 是一次发布决定：把一个目标组合推给一批设备。
type Campaign struct {
	ID        int       `json:"id"`
	Kind      string    `json:"kind"` // rollout | rollback
	Target    Combo     `json:"target"`
	DeviceIDs []string  `json:"device_ids"`
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note,omitempty"`
}

// Device 是服务端视角的设备状态。
type Device struct {
	ID         string            `json:"id"`
	Current    Combo             `json:"current"`     // 实际生效组合（整组合切换后更新）
	Applied    Combo             `json:"applied"`     // 各组件已确认 applied 的真实版本
	Target     Combo             `json:"target"`      // 当前活动战役的目标组合
	CampaignID int               `json:"campaign_id"` // 设备正在执行的战役
	Components map[string]string `json:"components"`  // 目标组合内各组件状态
	Blocked    []string          `json:"blocked"`     // 重新核对后的阻碍原因
	LastError  string            `json:"last_error"`
	LastSeen   *time.Time        `json:"last_seen,omitempty"`
	Online     bool              `json:"online"`
}

// HistoryEntry 是历史记录。发布决定与规则变化分开存储、分开查询。
type HistoryEntry struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

// PreviewView 是 Preview 对单台设备的评估结果。
type PreviewView struct {
	DeviceID string   `json:"device_id"`
	Feasible bool     `json:"feasible"`
	Reasons  []string `json:"reasons,omitempty"`
}

// DeviceView 是状态查询中的设备视图。
type DeviceView struct {
	Device
	WaitingFor []string `json:"waiting_for"` // 尚未 applied 的目标组件及其状态
	Complete   bool     `json:"complete"`    // 整个目标组合已生效
}

type persistedData struct {
	Versions   []ComponentVersion `json:"versions"`
	Rules      []Rule             `json:"rules"`
	Devices    map[string]*Device `json:"devices"`
	Campaigns  []Campaign         `json:"campaigns"`
	Decisions  []HistoryEntry     `json:"decisions"`
	RuleEvents []HistoryEntry     `json:"rule_events"`
}

// Store 持久化全部状态，重启后可继续核对组合状态。
type Store struct {
	mu         sync.Mutex
	dataDir    string
	versions   []ComponentVersion
	rules      []Rule
	devices    map[string]*Device
	campaigns  []Campaign
	decisions  []HistoryEntry
	ruleEvents []HistoryEntry
}

func NewStore(dataDir string) (*Store, error) {
	s := &Store{dataDir: dataDir, devices: map[string]*Device{}}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) filePath() string { return filepath.Join(s.dataDir, "compat-state.json") }

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
	s.versions = p.Versions
	s.rules = p.Rules
	s.campaigns = p.Campaigns
	s.decisions = p.Decisions
	s.ruleEvents = p.RuleEvents
	if p.Devices != nil {
		s.devices = p.Devices
	}
	return nil
}

func (s *Store) persistLocked() error {
	p := persistedData{
		Versions:   s.versions,
		Rules:      s.rules,
		Devices:    s.devices,
		Campaigns:  s.campaigns,
		Decisions:  s.decisions,
		RuleEvents: s.ruleEvents,
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

// RegisterVersion 登记一个组件版本（幂等）。
func (s *Store) RegisterVersion(component string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions {
		if v.Component == component && v.Version == version {
			return nil
		}
	}
	s.versions = append(s.versions, ComponentVersion{
		Component: component, Version: version, ReleasedAt: time.Now().UTC(),
	})
	s.ruleEvents = append(s.ruleEvents, HistoryEntry{
		At: time.Now().UTC(), Kind: "version_registered",
		Detail: fmt.Sprintf("登记版本 %s@%d", component, version),
	})
	return s.persistLocked()
}

// SetRule 新增或替换一条兼容约束，并触发未完成的设备重新核对。
func (s *Store) SetRule(rule Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rule.ID == "" || rule.Component == "" {
		return fmt.Errorf("规则缺少 id 或 component")
	}
	replaced := false
	for i, r := range s.rules {
		if r.ID == rule.ID {
			s.rules[i] = rule
			replaced = true
			break
		}
	}
	if !replaced {
		s.rules = append(s.rules, rule)
	}
	s.ruleEvents = append(s.ruleEvents, HistoryEntry{
		At: time.Now().UTC(), Kind: "rule_set",
		Detail: fmt.Sprintf("约束 %s: %s@%d 要求 %v", rule.ID, rule.Component, rule.Version, rule.Requires),
	})
	s.recheckLocked()
	return s.persistLocked()
}

// WithdrawVersion 撤回/恢复一个版本：已应用设备保留真实状态，
// 未开始设备重新核对，部分应用设备明确等待收尾项。
func (s *Store) WithdrawVersion(component string, version int, withdrawn bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i, v := range s.versions {
		if v.Component == component && v.Version == version {
			s.versions[i].Withdrawn = withdrawn
			found = true
		}
	}
	if !found {
		return fmt.Errorf("版本 %s@%d 不存在", component, version)
	}
	action := "撤回"
	if !withdrawn {
		action = "恢复"
	}
	s.ruleEvents = append(s.ruleEvents, HistoryEntry{
		At: time.Now().UTC(), Kind: "version_withdrawn",
		Detail: fmt.Sprintf("%s版本 %s@%d", action, component, version),
	})
	s.recheckLocked()
	return s.persistLocked()
}

// checkComboLocked 校验组合是否满足当前全部约束，返回阻碍原因列表。
func (s *Store) checkComboLocked(combo Combo) []string {
	var reasons []string
	names := make([]string, 0, len(combo))
	for name := range combo {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cv, ok := s.findVersionLocked(name, combo[name])
		if !ok {
			reasons = append(reasons, fmt.Sprintf("%s@%d 未发布", name, combo[name]))
			continue
		}
		if cv.Withdrawn {
			reasons = append(reasons, fmt.Sprintf("%s@%d 已撤回", name, combo[name]))
		}
	}
	for _, r := range s.rules {
		have, ok := combo[r.Component]
		if !ok || have != r.Version {
			continue
		}
		deps := make([]string, 0, len(r.Requires))
		for dep := range r.Requires {
			deps = append(deps, dep)
		}
		sort.Strings(deps)
		for _, dep := range deps {
			allowed := r.Requires[dep]
			actual, ok := combo[dep]
			if !ok {
				reasons = append(reasons, fmt.Sprintf("%s@%d 要求 %s，但组合中缺少该组件", r.Component, r.Version, dep))
				continue
			}
			okVersion := false
			for _, a := range allowed {
				if actual == a {
					okVersion = true
					break
				}
			}
			if !okVersion {
				reasons = append(reasons, fmt.Sprintf("%s@%d 要求 %s ∈ %v，实际为 %s@%d",
					r.Component, r.Version, dep, allowed, dep, actual))
			}
		}
	}
	return reasons
}

func (s *Store) findVersionLocked(component string, version int) (ComponentVersion, bool) {
	for _, v := range s.versions {
		if v.Component == component && v.Version == version {
			return v, true
		}
	}
	return ComponentVersion{}, false
}

// recheckLocked 在约束/版本变化后重新核对：未开始设备整体重判，
// 已有部分应用进度的设备保留真实状态，仅刷新阻碍信息。
func (s *Store) recheckLocked() {
	for _, d := range s.devices {
		if d.CampaignID == 0 || d.Target == nil {
			continue
		}
		if s.deviceCompleteLocked(d) {
			continue
		}
		d.Blocked = s.checkComboLocked(d.Target)
	}
}

func (s *Store) deviceCompleteLocked(d *Device) bool {
	if d.Target == nil {
		return false
	}
	for name := range d.Target {
		if d.Components[name] != CompApplied {
			return false
		}
	}
	return true
}

func (s *Store) deviceStartedLocked(d *Device) bool {
	for _, st := range d.Components {
		if st != CompPending {
			return true
		}
	}
	return false
}

// Preview 让操作者在圈选批次前看到每台设备的可行性与阻碍原因。
func (s *Store) Preview(target Combo, deviceIDs []string) ([]PreviewView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(deviceIDs) == 0 {
		for id := range s.devices {
			deviceIDs = append(deviceIDs, id)
		}
		sort.Strings(deviceIDs)
	}
	comboReasons := s.checkComboLocked(target)
	views := make([]PreviewView, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		d, ok := s.devices[id]
		v := PreviewView{DeviceID: id}
		v.Reasons = append(v.Reasons, comboReasons...)
		if !ok {
			v.Reasons = append(v.Reasons, "设备未注册")
		} else {
			if !d.Online {
				v.Reasons = append(v.Reasons, "设备离线")
			}
			if d.CampaignID != 0 && !s.deviceCompleteLocked(d) && s.deviceStartedLocked(d) {
				v.Reasons = append(v.Reasons,
					fmt.Sprintf("设备正在执行战役 #%d 且已有组件应用，需先收尾或显式换批", d.CampaignID))
			}
		}
		v.Feasible = len(v.Reasons) == 0
		views = append(views, v)
	}
	return views, nil
}

// StartCampaign 校验目标组合兼容后，把它推给选定的一批设备。
// kind 为 "rollout" 或 "rollback"；回退同样必须选择仍兼容的完整组合。
func (s *Store) StartCampaign(kind string, target Combo, deviceIDs []string, note string) (Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "" {
		kind = "rollout"
	}
	if kind != "rollout" && kind != "rollback" {
		return Campaign{}, fmt.Errorf("未知战役类型 %q", kind)
	}
	if len(deviceIDs) == 0 {
		return Campaign{}, fmt.Errorf("批次设备不能为空")
	}
	if len(target) == 0 {
		return Campaign{}, fmt.Errorf("目标组合不能为空")
	}
	if reasons := s.checkComboLocked(target); len(reasons) > 0 {
		return Campaign{}, fmt.Errorf("目标组合不满足当前兼容约束: %s", strings.Join(reasons, "; "))
	}
	for _, id := range deviceIDs {
		if _, ok := s.devices[id]; !ok {
			return Campaign{}, fmt.Errorf("设备 %s 未注册", id)
		}
	}
	camp := Campaign{
		ID:        len(s.campaigns) + 1,
		Kind:      kind,
		Target:    target.clone(),
		DeviceIDs: append([]string(nil), deviceIDs...),
		CreatedAt: time.Now().UTC(),
		Note:      note,
	}
	s.campaigns = append(s.campaigns, camp)
	for _, id := range deviceIDs {
		d := s.devices[id]
		d.Target = target.clone()
		d.CampaignID = camp.ID
		d.Components = map[string]string{}
		for name := range target {
			if d.Applied != nil && d.Applied[name] == target[name] {
				d.Components[name] = CompApplied // 设备上该组件已是目标版本，无需重复切换
			} else {
				d.Components[name] = CompPending
			}
		}
		d.Blocked = nil
		d.LastError = ""
	}
	s.decisions = append(s.decisions, HistoryEntry{
		At: time.Now().UTC(), Kind: kind,
		Detail: fmt.Sprintf("战役 #%d 目标 %s 设备 %v %s", camp.ID, target.Key(), deviceIDs, note),
	})
	if err := s.persistLocked(); err != nil {
		return Campaign{}, err
	}
	return camp, nil
}

// Heartbeat 设备心跳/注册：上报实际生效组合，服务端据此对账。
func (s *Store) Heartbeat(deviceID string, current Combo, online bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.ensureDeviceLocked(deviceID)
	now := time.Now().UTC()
	d.LastSeen = &now
	d.Online = online
	if current != nil {
		d.Current = current.clone()
		d.Applied = current.clone()
	}
	return s.persistLocked()
}

func (s *Store) ensureDeviceLocked(id string) *Device {
	d, ok := s.devices[id]
	if !ok {
		d = &Device{ID: id, Current: Combo{}, Applied: Combo{}, Components: map[string]string{}}
		s.devices[id] = d
	}
	return d
}

// AckRequest 是设备对单个组件的确认。
type AckRequest struct {
	DeviceID   string
	CampaignID int
	Component  string
	Kind       string // downloaded | validated | applied | failed
	Reason     string
}

// Ack 记录单个组件的确认。单个组件 downloaded/validated/applied
// 只推进该组件自身状态；只有目标组合内全部组件 applied，
// 设备才算整体切换到新组合。过期战役的迟到确认被拒绝。
func (s *Store) Ack(req AckRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[req.DeviceID]
	if !ok {
		return fmt.Errorf("设备 %s 未注册", req.DeviceID)
	}
	if d.CampaignID == 0 {
		return fmt.Errorf("设备 %s 当前没有活动战役", req.DeviceID)
	}
	if req.CampaignID != d.CampaignID {
		return fmt.Errorf("迟到确认：战役 #%d 已过期，设备当前战役为 #%d", req.CampaignID, d.CampaignID)
	}
	targetVer, ok := d.Target[req.Component]
	if !ok {
		return fmt.Errorf("组件 %s 不在目标组合中", req.Component)
	}
	switch req.Kind {
	case CompDownloaded:
		if d.Components[req.Component] == CompPending {
			d.Components[req.Component] = CompDownloaded
		}
	case CompValidated:
		st := d.Components[req.Component]
		if st == CompPending || st == CompDownloaded {
			d.Components[req.Component] = CompValidated
		}
	case CompApplied:
		d.Components[req.Component] = CompApplied
		d.LastError = ""
		if d.Applied == nil {
			d.Applied = Combo{}
		}
		d.Applied[req.Component] = targetVer
		if s.deviceCompleteLocked(d) {
			d.Current = d.Target.clone()
			d.Blocked = nil
		}
	case CompFailed:
		if d.Components[req.Component] != CompApplied {
			d.Components[req.Component] = CompFailed
			d.LastError = fmt.Sprintf("%s@%d: %s", req.Component, targetVer, req.Reason)
		}
	default:
		return fmt.Errorf("未知确认类型 %q", req.Kind)
	}
	return s.persistLocked()
}

// Status 返回整体状态：每台设备的实际组合、目标组合、各组件状态、
// 等待收尾项与阻碍原因。
func (s *Store) Status() (map[string]DeviceView, []Campaign) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]DeviceView{}
	for id, d := range s.devices {
		view := DeviceView{Device: *d}
		if d.Target != nil {
			names := make([]string, 0, len(d.Target))
			for name := range d.Target {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if d.Components[name] != CompApplied {
					view.WaitingFor = append(view.WaitingFor,
						fmt.Sprintf("%s@%d(%s)", name, d.Target[name], d.Components[name]))
				}
			}
		}
		view.Complete = s.deviceCompleteLocked(d)
		out[id] = view
	}
	return out, append([]Campaign(nil), s.campaigns...)
}

// Decisions 返回发布决定历史（战役创建、回退）。
func (s *Store) Decisions() []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HistoryEntry(nil), s.decisions...)
}

// RuleEvents 返回规则变化历史（版本登记/撤回、约束修改）。
func (s *Store) RuleEvents() []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HistoryEntry(nil), s.ruleEvents...)
}

// Versions 返回全部已登记组件版本。
func (s *Store) Versions() []ComponentVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ComponentVersion(nil), s.versions...)
}

// Rules 返回当前生效的兼容约束。
func (s *Store) Rules() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Rule(nil), s.rules...)
}
