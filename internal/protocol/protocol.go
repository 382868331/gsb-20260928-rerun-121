package protocol

import "time"

// 客户端针对某个版本的状态。状态只能单调推进，不允许回退。
const (
	StatePending    = "pending"    // 有新版本待处理
	StateDownloaded = "downloaded" // 已下载完整快照，尚未应用
	StateVerified   = "verified"   // 已校验通过（校验和 + 业务校验），尚未应用
	StateApplied    = "applied"    // 已实际应用成功
	StateFailed     = "failed"     // 应用失败（保留原因与原版本）
)

// 确认类型：下载、校验、应用三种确认严格区分。
const (
	AckDownloaded = "downloaded"
	AckVerified   = "verified"
	AckApplied    = "applied"
	AckFailed     = "failed"
)

// 放量批次状态。
const (
	RolloutRunning = "running" // 正在逐批放量
	RolloutHalted  = "halted"  // 某批失败/回退，后续批次自动停止
	RolloutDone    = "done"    // 全部批次完成
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

// VersionRecord 是客户端对某个版本的历史记录（失败历史永久保留）。
type VersionRecord struct {
	Version   int        `json:"version"`
	State     string     `json:"state"`
	LastError string     `json:"last_error,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// AckRequest 客户端确认。
type AckRequest struct {
	ClientID string `json:"client_id"`
	Version  int    `json:"version"`
	Kind     string `json:"kind"` // downloaded | applied | failed
	Reason   string `json:"reason,omitempty"`
}

// ReconcileResponse 注册/心跳返回：客户端应对齐到的目标完整版本。
// 注意是“目标版本”而非“最新版本”：分批放量中未到批次的客户端、
// 以及回退中的客户端，目标版本都可能是旧版本。
type ReconcileResponse struct {
	Latest *Release `json:"latest"` // 目标版本完整快照，可能为 nil
	// TargetVersion 服务端认为该客户端应达到的版本。
	TargetVersion int `json:"target_version"`
}

// RolloutView 分批放量进度。
type RolloutView struct {
	ReleaseVersion int        `json:"release_version"`
	Batches        [][]string `json:"batches"`
	Current        int        `json:"current"` // 当前批次下标
	Status         string     `json:"status"`  // running | halted | done
	HaltReason     string     `json:"halt_reason,omitempty"`
}

// StatusResponse 整体状态（管理/测试用）。
type StatusResponse struct {
	LatestVersion int                        `json:"latest_version"`
	Rollout       *RolloutView               `json:"rollout,omitempty"`
	Clients       []ClientView               `json:"clients"`
	History       map[string][]VersionRecord `json:"history,omitempty"` // 每客户端版本历史（含失败）
}

// PublishRequest 发布新版本。batches 为空时等价于单批全量。
type PublishRequest struct {
	Content map[string]interface{} `json:"content"`
	Batches [][]string             `json:"batches,omitempty"` // 分批放量的客户端分组
}

// RollbackRequest 发布明确的回退目标版本。
type RollbackRequest struct {
	Version int `json:"version"`
}
