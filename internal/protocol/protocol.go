package protocol

// Config 是一份完整配置。系统只分发整份配置（快照），不做增量或文件同步。
type Config struct {
	ID       string `json:"id"`
	Sequence int    `json:"sequence"`
	Content  any    `json:"content"`
}

// ClientStage 表示客户端汇报的阶段。
const (
	StageDownloaded = "downloaded" // 仅表示已下载完整快照，不代表应用成功
	StageApplied    = "applied"    // 已校验并原子切换成功
	StageFailed     = "failed"     // 应用失败，原版本必须保留
)

type RegisterRequest struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type Manifest struct {
	DesiredID  string `json:"desiredId"`
	DesiredSeq int    `json:"desiredSeq"`
}

type ReportRequest struct {
	ClientID        string `json:"clientId"`
	Stage           string `json:"stage"`
	VersionID       string `json:"versionId"`
	ActiveVersionID string `json:"activeVersionId"`
	Reason          string `json:"reason,omitempty"`
}

type ActualResponse struct {
	ClientID      string `json:"clientId"`
	ActiveVersion string `json:"activeVersion"`
}

// ClientState 是服务端记录的单个客户端状态（持久化）。
type ClientState struct {
	ID            string `json:"id"`
	Addr          string `json:"addr"`
	DownloadedID  string `json:"downloadedId"`
	AppliedID     string `json:"appliedId"`
	ObservedID    string `json:"observedId"`
	PendingID     string `json:"pendingId"` // 期望但尚未成功应用的版本
	FailureReason string `json:"failureReason"`
}

type StatusResponse struct {
	DesiredID string                 `json:"desiredId"`
	Clients   map[string]ClientState `json:"clients"`
}
