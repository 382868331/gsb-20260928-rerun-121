package protocol

import "time"

// 客户端针对某个版本的状态。状态只能单调推进，不允许回退。
const (
	StatePending    = "pending"    // 有新版本待处理
	StateDownloaded = "downloaded" // 已下载完整快照，尚未应用
	StateApplied    = "applied"    // 已实际应用成功
	StateFailed     = "failed"     // 应用失败（保留原因与原版本）
)

// 确认类型：下载确认与应用确认严格区分。
const (
	AckDownloaded = "downloaded"
	AckApplied    = "applied"
	AckFailed     = "failed"
)

// Release 是一份完整配置快照（整份配置，非增量）。
type Release struct {
	Version   int                    `json:"version"`
	Content   map[string]interface{} `json:"content"`
	Checksum  string                 `json:"checksum"`
	CreatedAt time.Time              `json:"created_at"`
}

// RegisterRequest 客户端注册 / 心跳对账请求。
// ActiveVersion 是客户端“当前实际生效”的版本，由客户端自报。
type RegisterRequest struct {
	ClientID      string `json:"client_id"`
	ActiveVersion int    `json:"active_version"`
}

// ClientView 服务端视角下的客户端状态。
type ClientView struct {
	ClientID      string     `json:"client_id"`
	ActiveVersion int        `json:"active_version"` // 客户端实际上报的生效版本
	TargetVersion int        `json:"target_version"` // 应对齐到的版本
	State         string     `json:"state"`          // 针对 target_version 的状态
	LastError     string     `json:"last_error"`
	LastSeen      *time.Time `json:"last_seen,omitempty"`
	Online        bool       `json:"online"`
}

// AckRequest 客户端确认。
type AckRequest struct {
	ClientID string `json:"client_id"`
	Version  int    `json:"version"`
	Kind     string `json:"kind"` // downloaded | applied | failed
	Reason   string `json:"reason,omitempty"`
}

// ReconcileResponse 注册/心跳返回：客户端应处理的最新完整版本。
type ReconcileResponse struct {
	Latest *Release `json:"latest"` // 最新完整快照，可能为 nil
	// TargetVersion 服务端认为该客户端应达到的版本。
	TargetVersion int `json:"target_version"`
}

// StatusResponse 整体状态（管理/测试用）。
type StatusResponse struct {
	LatestVersion int          `json:"latest_version"`
	Clients       []ClientView `json:"clients"`
}

// PublishRequest 发布新版本。
type PublishRequest struct {
	Content map[string]interface{} `json:"content"`
}
