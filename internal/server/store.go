package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"configdist/internal/protocol"
)

// clientState 是服务端记录的单个客户端状态。
type clientState struct {
	ActiveVersion int        `json:"active_version"` // 客户端自报的实际生效版本
	State         string     `json:"state"`          // 针对 TargetVersion 的确认状态
	LastError     string     `json:"last_error"`
	LastSeen      *time.Time `json:"last_seen,omitempty"`
}

// persistedData 是磁盘持久化格式，保证服务端可重启恢复。
type persistedData struct {
	Releases []protocol.Release      `json:"releases"`
	Clients  map[string]*clientState `json:"clients"`
}

// Store 保存所有版本与客户端状态，并发安全且持久化。
type Store struct {
	mu       sync.Mutex
	dataDir  string
	releases []protocol.Release
	clients  map[string]*clientState
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
	return nil
}

// persist 调用方需持锁。先写临时文件再原子改名。
func (s *Store) persistLocked() error {
	p := persistedData{Releases: s.releases, Clients: s.clients}
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
	if len(s.releases) == 0 {
		return protocol.Release{}, false
	}
	return s.releases[len(s.releases)-1], true
}

func (s *Store) GetRelease(version int) (protocol.Release, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.releases {
		if r.Version == version {
			return r, true
		}
	}
	return protocol.Release{}, false
}

// Publish 追加一份新的完整配置快照。
// 新发布会让所有未在该版本上的客户端回到 pending（待处理）。
func (s *Store) Publish(content map[string]interface{}) (protocol.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := protocol.Release{
		Version:   len(s.releases) + 1,
		Content:   content,
		Checksum:  checksumOf(content),
		CreatedAt: time.Now().UTC(),
	}
	s.releases = append(s.releases, rel)
	for _, c := range s.clients {
		if c.ActiveVersion < rel.Version {
			// 有了更新版本：无论上次是 downloaded 还是 failed，都重新待处理；
			// 保留 LastError 作为历史失败原因可见。
			c.State = protocol.StatePending
		}
	}
	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, err
	}
	return rel, nil
}

// Touch 处理注册/心跳：以客户端自报的实际版本为准进行对账。
// 返回最新完整快照（供客户端直接拉取，跳过中间版本）。
func (s *Store) Touch(clientID string, activeVersion int) (protocol.Release, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	c, ok := s.clients[clientID]
	if !ok {
		c = &clientState{State: protocol.StatePending}
		s.clients[clientID] = c
	}
	c.LastSeen = &now

	latest, hasLatest := protocol.Release{}, false
	if len(s.releases) > 0 {
		latest = s.releases[len(s.releases)-1]
		hasLatest = true
	}

	if hasLatest {
		switch {
		case activeVersion >= latest.Version:
			// 客户端实际已是最新：以事实为准，修正为 applied（单调推进的终点）。
			c.ActiveVersion = latest.Version
			c.State = protocol.StateApplied
			c.LastError = ""
		default:
			// 重启/断连恢复：客户端实际版本落后，待处理；不臆测它已应用。
			c.ActiveVersion = activeVersion
			if c.State == protocol.StateApplied {
				c.State = protocol.StatePending
			}
		}
	} else {
		c.ActiveVersion = activeVersion
	}

	if err := s.persistLocked(); err != nil {
		return protocol.Release{}, false, err
	}
	return latest, hasLatest, nil
}

// Ack 记录客户端确认。核心规则：
//  1. downloaded 只表示下载完成，不算应用成功；
//  2. applied 才推进 ActiveVersion；
//  3. 状态单调推进，重复/乱序确认不回退。
func (s *Store) Ack(req protocol.AckRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[req.ClientID]
	if !ok {
		c = &clientState{}
		s.clients[req.ClientID] = c
	}
	// 该确认只针对目标（最新）版本；过期版本确认直接忽略，避免回退。
	latestVersion := 0
	if len(s.releases) > 0 {
		latestVersion = s.releases[len(s.releases)-1].Version
	}
	if req.Version != latestVersion {
		// 旧版本的迟到确认不影响当前状态。
		return s.persistLocked()
	}

	switch req.Kind {
	case protocol.AckDownloaded:
		if c.State == protocol.StatePending {
			c.State = protocol.StateDownloaded
		}
		// 已 downloaded/applied/failed 时，重复 downloaded 不回退。
	case protocol.AckApplied:
		// applied 是成功终态，任何当前状态都可推进到它。
		c.State = protocol.StateApplied
		c.ActiveVersion = req.Version
		c.LastError = ""
	case protocol.AckFailed:
		// 只有尚未成功时才记录失败；已 applied 不允许被失败覆盖。
		if c.State != protocol.StateApplied {
			c.State = protocol.StateFailed
			c.LastError = req.Reason
		}
	default:
		return fmt.Errorf("未知确认类型 %q", req.Kind)
	}
	return s.persistLocked()
}

// Snapshot 返回整体状态视图。
func (s *Store) Snapshot(online func(id string) bool) protocol.StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	latestVersion := 0
	if len(s.releases) > 0 {
		latestVersion = s.releases[len(s.releases)-1].Version
	}
	views := make([]protocol.ClientView, 0, len(s.clients))
	for id, c := range s.clients {
		state := c.State
		target := latestVersion
		if c.ActiveVersion >= latestVersion && latestVersion > 0 {
			state = protocol.StateApplied
			target = c.ActiveVersion
		}
		v := protocol.ClientView{
			ClientID:      id,
			ActiveVersion: c.ActiveVersion,
			TargetVersion: target,
			State:         state,
			LastError:     c.LastError,
			LastSeen:      c.LastSeen,
		}
		if online != nil {
			v.Online = online(id)
		}
		views = append(views, v)
	}
	return protocol.StatusResponse{LatestVersion: latestVersion, Clients: views}
}

func checksumOf(content map[string]interface{}) string {
	return stableChecksum(content)
}
