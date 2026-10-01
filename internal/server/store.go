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
type clientState struct {
	ActiveVersion int                      `json:"active_version"`
	State         string                   `json:"state"`
	LastError     string                   `json:"last_error"`
	Failures      []protocol.FailureRecord `json:"failures,omitempty"`
	LastSeen      *time.Time               `json:"last_seen,omitempty"`
}

type persistedData struct {
	Releases   []protocol.Release      `json:"releases"`
	Clients    map[string]*clientState `json:"clients"`
	Deployment protocol.Deployment     `json:"deployment"`
}

type Store struct {
	mu         sync.Mutex
	dataDir    string
	releases   []protocol.Release
	clients    map[string]*clientState
	deployment protocol.Deployment
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
	s.deployment = p.Deployment
	if p.Clients != nil {
		s.clients = p.Clients
	}
	return nil
}

func (s *Store) persistLocked() error {
	p := persistedData{Releases: s.releases, Clients: s.clients, Deployment: s.deployment}
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

func (s *Store) Latest() (protocol.Release, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latestLocked()
}

func (s *Store) latestLocked() (protocol.Release, bool) {
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

// Publish 追加完整配置快照，并可选只让首批客户端看到该目标。
// BatchClientIDs == nil 表示全量；显式切片表示受限批次。
func (s *Store) Publish(req protocol.PublishRequest) (protocol.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := protocol.Release{
		Version:   len(s.releases) + 1,
		Content:   req.Content,
		Checksum:  checksumOf(req.Content),
		CreatedAt: time.Now().UTC(),
	}
	s.releases = append(s.releases, rel)
	s.deployment = protocol.Deployment{
		Mode:           "rollout",
		TargetVersion:  rel.Version,
		BatchClientIDs: append([]string(nil), req.BatchClientIDs...),
		StartedAt:      rel.CreatedAt,
	}
	if req.BatchClientIDs != nil {
		for _, id := range req.BatchClientIDs {
			c := s.ensureClientLocked(id)
			if c.ActiveVersion != rel.Version {
				c.State = protocol.StatePending
			}
		}
	} else {
		for _, c := range s.clients {
			if c.ActiveVersion != rel.Version {
				c.State = protocol.StatePending
			}
		}
	}
	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, err
	}
	return rel, nil
}

// PromoteBatch 把后续批次放入当前版本目标。当前批次一旦出现应用失败，
// 放量会被冻结，必须先显式回退或发布新版本。
func (s *Store) PromoteBatch(targetVersion int, clientIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deployment.TargetVersion != targetVersion {
		return fmt.Errorf("版本 v%d 不是当前部署目标", targetVersion)
	}
	if s.deployment.Mode != "rollout" {
		return fmt.Errorf("当前不是分批发布")
	}
	if s.deployment.Stopped {
		return fmt.Errorf("放量已停止: %s", s.deployment.StopReason)
	}
	if len(clientIDs) == 0 {
		return fmt.Errorf("批次客户端不能为空")
	}
	s.deployment.BatchClientIDs = append([]string(nil), clientIDs...)
	for _, id := range clientIDs {
		c := s.ensureClientLocked(id)
		if c.ActiveVersion != targetVersion {
			c.State = protocol.StatePending
		}
	}
	return s.persistLocked()
}

// Rollback 显式发布旧版本作为新的部署目标。失败历史只追加，不覆盖。
func (s *Store) Rollback(targetVersion int) (protocol.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, ok := s.getReleaseLocked(targetVersion)
	if !ok {
		return protocol.Release{}, fmt.Errorf("回退目标 v%d 不存在", targetVersion)
	}
	latest, _ := s.latestLocked()
	if targetVersion >= latest.Version {
		return protocol.Release{}, fmt.Errorf("回退目标必须早于当前最新版本 v%d", latest.Version)
	}
	s.deployment = protocol.Deployment{
		Mode:           "rollback",
		TargetVersion:  targetVersion,
		BatchClientIDs: nil,
		StartedAt:      time.Now().UTC(),
	}
	for _, c := range s.clients {
		if c.ActiveVersion != targetVersion {
			c.State = protocol.StatePending
		}
	}
	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, err
	}
	return rel, nil
}

// Touch 以客户端重启/心跳时自报的实际生效版本对账，并仅返回该客户端
// 当前批次允许获取的目标快照。
func (s *Store) Touch(clientID string, activeVersion int) (protocol.Release, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	c := s.ensureClientLocked(clientID)
	c.LastSeen = &now
	c.ActiveVersion = activeVersion

	targetVersion := s.deployment.TargetVersion
	if targetVersion == 0 {
		if err := s.persistLocked(); err != nil {
			return protocol.Release{}, false, err
		}
		return protocol.Release{}, false, nil
	}

	if activeVersion == targetVersion {
		c.State = protocol.StateApplied
	} else if s.inCurrentBatchLocked(clientID) {
		c.State = protocol.StatePending
	}

	if !s.inCurrentBatchLocked(clientID) || activeVersion == targetVersion {
		if err := s.persistLocked(); err != nil {
			return protocol.Release{}, false, err
		}
		return protocol.Release{}, false, nil
	}
	rel, ok := s.getReleaseLocked(targetVersion)
	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, false, err
	}
	return rel, ok, nil
}

// Ack 分别记录 downloaded、validated、applied、failed。
// downloaded/validated 永远不能代表真正应用；旧目标的迟到确认直接拒绝。
func (s *Store) Ack(req protocol.AckRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deployment.TargetVersion == 0 {
		return fmt.Errorf("尚无部署目标")
	}
	if req.Version != s.deployment.TargetVersion {
		return fmt.Errorf("v%d 是过期目标，当前目标为 v%d", req.Version, s.deployment.TargetVersion)
	}
	if !s.inCurrentBatchLocked(req.ClientID) {
		return fmt.Errorf("客户端 %s 尚未进入当前批次", req.ClientID)
	}
	c := s.ensureClientLocked(req.ClientID)
	switch req.Kind {
	case protocol.AckDownloaded:
		if c.State == protocol.StatePending {
			c.State = protocol.StateDownloaded
		}
	case protocol.AckValidated:
		if c.State == protocol.StateDownloaded || c.State == protocol.StatePending {
			c.State = protocol.StateValidated
		}
	case protocol.AckApplied:
		c.State = protocol.StateApplied
		c.ActiveVersion = req.Version
		c.LastError = ""
	case protocol.AckFailed:
		if c.State != protocol.StateApplied {
			c.State = protocol.StateFailed
			c.LastError = req.Reason
			c.Failures = append(c.Failures, protocol.FailureRecord{
				Version:  req.Version,
				Reason:   req.Reason,
				FailedAt: time.Now().UTC(),
			})
			s.deployment.Stopped = true
			s.deployment.StopReason = fmt.Sprintf("client %s failed v%d: %s", req.ClientID, req.Version, req.Reason)
		}
	default:
		return fmt.Errorf("未知确认类型 %q", req.Kind)
	}
	return s.persistLocked()
}

func (s *Store) Snapshot(online func(id string) bool) protocol.StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	latestVersion := 0
	if rel, ok := s.latestLocked(); ok {
		latestVersion = rel.Version
	}
	ids := make([]string, 0, len(s.clients))
	for id := range s.clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	views := make([]protocol.ClientView, 0, len(ids))
	for _, id := range ids {
		c := s.clients[id]
		inBatch := s.inCurrentBatchLocked(id)
		state := c.State
		if s.deployment.TargetVersion != 0 && !inBatch && c.ActiveVersion != s.deployment.TargetVersion {
			state = "waiting"
		}
		v := protocol.ClientView{
			ClientID:       id,
			ActiveVersion:  c.ActiveVersion,
			TargetVersion:  s.deployment.TargetVersion,
			State:          state,
			LastError:      c.LastError,
			Failures:       append([]protocol.FailureRecord(nil), c.Failures...),
			LastSeen:       c.LastSeen,
			InCurrentBatch: inBatch,
		}
		if online != nil {
			v.Online = online(id)
		}
		views = append(views, v)
	}
	return protocol.StatusResponse{LatestVersion: latestVersion, Deployment: s.deployment, Clients: views}
}

func (s *Store) ensureClientLocked(id string) *clientState {
	c, ok := s.clients[id]
	if !ok {
		c = &clientState{State: protocol.StatePending}
		s.clients[id] = c
	}
	return c
}

func (s *Store) inCurrentBatchLocked(id string) bool {
	if s.deployment.TargetVersion == 0 {
		return false
	}
	if s.deployment.BatchClientIDs == nil {
		return true
	}
	for _, candidate := range s.deployment.BatchClientIDs {
		if candidate == id {
			return true
		}
	}
	return false
}

func checksumOf(content map[string]interface{}) string {
	return stableChecksum(content)
}
