package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"configdist/internal/protocol"
)

// clientState 是服务端记录的单个客户端状态。
// 每个版本的历史（含失败原因）独立保存在 Records 中，永不被覆盖。
type clientState struct {
	ActiveVersion int                             `json:"active_version"` // 客户端自报的实际生效版本
	TargetVersion int                             `json:"target_version"` // 当前应对齐的版本
	LastSeen      *time.Time                      `json:"last_seen,omitempty"`
	Records       map[int]*protocol.VersionRecord `json:"records"` // 按版本的历史记录
}

// rolloutState 一次分批放量的进度。
type rolloutState struct {
	ReleaseVersion int        `json:"release_version"`
	Batches        [][]string `json:"batches"`
	Current        int        `json:"current"`
	Status         string     `json:"status"`
	HaltReason     string     `json:"halt_reason,omitempty"`
}

// persistedData 是磁盘持久化格式，保证服务端可重启恢复。
type persistedData struct {
	Releases     []protocol.Release      `json:"releases"`
	Clients      map[string]*clientState `json:"clients"`
	Rollout      *rolloutState           `json:"rollout,omitempty"`
	PinnedTarget int                     `json:"pinned_target,omitempty"` // 回退目标版本（0 表示跟随最新）
}

// Store 保存所有版本与客户端状态，并发安全且持久化。
type Store struct {
	mu           sync.Mutex
	dataDir      string
	releases     []protocol.Release
	clients      map[string]*clientState
	rollout      *rolloutState
	pinnedTarget int
}

func NewStore(dataDir string) (*Store, error) {
	s := &Store{
		dataDir: dataDir,
		clients: map[string]*clientState{},
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) filePath() string { return filepath.Join(s.dataDir, "state.json") }

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
	s.releases = p.Releases
	if p.Clients != nil {
		s.clients = p.Clients
	}
	s.rollout = p.Rollout
	s.pinnedTarget = p.PinnedTarget
	return nil
}

// persist 调用方需持锁。先写临时文件再原子改名。
func (s *Store) persistLocked() error {
	p := persistedData{
		Releases:     s.releases,
		Clients:      s.clients,
		Rollout:      s.rollout,
		PinnedTarget: s.pinnedTarget,
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

func (s *Store) latestVersionLocked() int {
	if len(s.releases) == 0 {
		return 0
	}
	return s.releases[len(s.releases)-1].Version
}

func (s *Store) Latest() (protocol.Release, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.releases) == 0 {
		return protocol.Release{}, false
	}
	return s.releases[len(s.releases)-1], true
}

func (s *Store) GetRelease(version int) (protocol.Release, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getReleaseLocked(version)
}

func (s *Store) getReleaseLocked(version int) (protocol.Release, bool) {
	for _, r := range s.releases {
		if r.Version == version {
			return r, true
		}
	}
	return protocol.Release{}, false
}

// recLocked 取客户端对某版本的历史记录，不存在则新建 pending。
func recLocked(c *clientState, version int) *protocol.VersionRecord {
	if c.Records == nil {
		c.Records = map[int]*protocol.VersionRecord{}
	}
	r, ok := c.Records[version]
	if !ok {
		r = &protocol.VersionRecord{Version: version, State: protocol.StatePending}
		c.Records[version] = r
	}
	return r
}

// stateRank 单调推进的等级；failed 不在成功链上，单独处理。
func stateRank(state string) int {
	switch state {
	case protocol.StateDownloaded:
		return 1
	case protocol.StateVerified:
		return 2
	case protocol.StateApplied:
		return 3
	default:
		return 0
	}
}

// Publish 追加一份新的完整配置快照，并按 batches 分批放量。
// batches 为空时等价于单批全量；未列入任何批次的已知客户端
// 自动归入最后一个隐含批次。首批客户端目标立即切到新版本，
// 后续批次等当前批全部 applied 后才放行；任一批出现 failed
// 即自动停止后续批次。
func (s *Store) Publish(content map[string]interface{}, batches [][]string) (protocol.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := protocol.Release{
		Version:   len(s.releases) + 1,
		Content:   content,
		Checksum:  checksumOf(content),
		CreatedAt: time.Now().UTC(),
	}
	s.releases = append(s.releases, rel)
	s.pinnedTarget = 0 // 新发布解除回退钉住

	// 未列入批次的已知客户端归入隐含末批。
	assigned := map[string]bool{}
	for _, b := range batches {
		for _, id := range b {
			assigned[id] = true
		}
	}
	var rest []string
	for id := range s.clients {
		if !assigned[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	if len(rest) > 0 {
		batches = append(append([][]string{}, batches...), rest)
	}
	if len(batches) == 0 {
		batches = [][]string{{}}
	}

	s.rollout = &rolloutState{
		ReleaseVersion: rel.Version,
		Batches:        batches,
		Current:        0,
		Status:         protocol.RolloutRunning,
	}
	// 首批放行。
	for _, id := range batches[0] {
		if c, ok := s.clients[id]; ok {
			c.TargetVersion = rel.Version
		}
	}
	s.evaluateRolloutLocked()
	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, err
	}
	return rel, nil
}

// Rollback 发布明确的回退目标：所有客户端目标版本改为 version，
// 进行中的放量立即停止。失败历史保留在 Records 中不被覆盖。
func (s *Store) Rollback(version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.getReleaseLocked(version); !ok {
		return fmt.Errorf("回退目标版本 v%d 不存在", version)
	}
	if s.rollout != nil && s.rollout.Status == protocol.RolloutRunning {
		s.rollout.Status = protocol.RolloutHalted
		s.rollout.HaltReason = fmt.Sprintf("回退到 v%d，放量停止", version)
	}
	s.pinnedTarget = version
	for _, c := range s.clients {
		c.TargetVersion = version
	}
	return s.persistLocked()
}

// evaluateRolloutLocked 检查当前批次：全部 applied 则放行下一批；
// 任一 failed 则自动停止后续批次。调用方需持锁。
func (s *Store) evaluateRolloutLocked() {
	r := s.rollout
	if r == nil || r.Status != protocol.RolloutRunning {
		return
	}
	for r.Current < len(r.Batches) {
		batch := r.Batches[r.Current]
		allApplied := true
		for _, id := range batch {
			c, ok := s.clients[id]
			if !ok {
				allApplied = false
				continue
			}
			rec := recLocked(c, r.ReleaseVersion)
			if rec.State == protocol.StateFailed {
				r.Status = protocol.RolloutHalted
				r.HaltReason = fmt.Sprintf("批次 %d 中 %s 应用失败: %s；后续批次已自动停止",
					r.Current+1, id, rec.LastError)
				return
			}
			if rec.State != protocol.StateApplied {
				allApplied = false
			}
		}
		if !allApplied {
			return
		}
		r.Current++
		if r.Current < len(r.Batches) {
			for _, id := range r.Batches[r.Current] {
				if c, ok := s.clients[id]; ok {
					c.TargetVersion = r.ReleaseVersion
				}
			}
		}
	}
	r.Status = protocol.RolloutDone
}

// Touch 处理注册/心跳：以客户端自报的实际版本为准进行对账。
// 返回该客户端应对齐的目标版本完整快照（可能是旧版本，例如回退中）。
func (s *Store) Touch(clientID string, activeVersion int) (protocol.Release, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	c, ok := s.clients[clientID]
	if !ok {
		c = &clientState{Records: map[int]*protocol.VersionRecord{}}
		// 新客户端目标：回退钉住版本，否则最新版本。
		if s.pinnedTarget > 0 {
			c.TargetVersion = s.pinnedTarget
		} else {
			c.TargetVersion = s.latestVersionLocked()
		}
		s.clients[clientID] = c
	}
	c.LastSeen = &now
	c.ActiveVersion = activeVersion

	// 对账：以客户端实际生效版本为准修正目标版本记录。
	rec := recLocked(c, c.TargetVersion)
	if c.TargetVersion > 0 && activeVersion == c.TargetVersion {
		// 实际已达标：确认丢失的场景在此补记 applied。
		rec.State = protocol.StateApplied
		rec.LastError = ""
	} else if rec.State == protocol.StateApplied {
		// 曾记 applied 但实际并未生效（如重启后回退）：回到待处理。
		rec.State = protocol.StatePending
	}

	s.evaluateRolloutLocked()
	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, false, err
	}
	rel, has := s.getReleaseLocked(c.TargetVersion)
	return rel, has, nil
}

// Ack 记录客户端确认。核心规则：
//  1. downloaded / verified 只表示阶段性进展，不算应用成功；
//  2. applied 才推进 ActiveVersion；
//  3. 只接受针对当前目标版本的确认：迟到的旧版本确认直接忽略，
//     不会把回退中的客户端重新标成新版本已完成；
//  4. 状态单调推进，重复/乱序确认不回退；
//  5. 每批次确认后评估放量进度，失败即自动停止后续批次。
func (s *Store) Ack(req protocol.AckRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[req.ClientID]
	if !ok {
		c = &clientState{Records: map[int]*protocol.VersionRecord{}}
		s.clients[req.ClientID] = c
	}
	if req.Version != c.TargetVersion {
		// 非当前目标版本的迟到确认：忽略，不触碰任何状态与历史。
		return s.persistLocked()
	}
	rec := recLocked(c, req.Version)
	now := time.Now().UTC()
	rec.UpdatedAt = &now

	switch req.Kind {
	case protocol.AckDownloaded, protocol.AckVerified:
		// 仅按等级单调推进；applied/failed 不被阶段确认覆盖。
		if stateRank(req.Kind) > stateRank(rec.State) {
			rec.State = req.Kind
		}
	case protocol.AckApplied:
		// applied 是成功终态，任何当前状态都可推进到它。
		rec.State = protocol.StateApplied
		rec.LastError = ""
		c.ActiveVersion = req.Version
	case protocol.AckFailed:
		// 只有尚未成功时才记录失败；已 applied 不允许被失败覆盖。
		if rec.State != protocol.StateApplied {
			rec.State = protocol.StateFailed
			rec.LastError = req.Reason
		}
	default:
		return fmt.Errorf("未知确认类型 %q", req.Kind)
	}
	s.evaluateRolloutLocked()
	return s.persistLocked()
}

// Snapshot 返回整体状态视图（含放量进度与每客户端版本历史）。
func (s *Store) Snapshot(online func(id string) bool) protocol.StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make([]protocol.ClientView, 0, len(s.clients))
	history := map[string][]protocol.VersionRecord{}
	for id, c := range s.clients {
		rec := recLocked(c, c.TargetVersion)
		v := protocol.ClientView{
			ClientID:      id,
			ActiveVersion: c.ActiveVersion,
			TargetVersion: c.TargetVersion,
			State:         rec.State,
			LastError:     rec.LastError,
			LastSeen:      c.LastSeen,
		}
		if online != nil {
			v.Online = online(id)
		}
		views = append(views, v)
		recs := make([]protocol.VersionRecord, 0, len(c.Records))
		for _, r := range c.Records {
			recs = append(recs, *r)
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].Version < recs[j].Version })
		history[id] = recs
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ClientID < views[j].ClientID })
	resp := protocol.StatusResponse{
		LatestVersion: s.latestVersionLocked(),
		Clients:       views,
		History:       history,
	}
	if s.rollout != nil {
		resp.Rollout = &protocol.RolloutView{
			ReleaseVersion: s.rollout.ReleaseVersion,
			Batches:        s.rollout.Batches,
			Current:        s.rollout.Current,
			Status:         s.rollout.Status,
			HaltReason:     s.rollout.HaltReason,
		}
	}
	return resp
}

func checksumOf(content map[string]interface{}) string {
	return stableChecksum(content)
}
