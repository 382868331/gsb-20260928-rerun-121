package combo

import (
	"fmt"
	"sort"
	"time"
)

// 组件级确认类型：下载、验证与真正应用严格区分。
// 单个组件 downloaded/validated 都不能代表整个组合可用。
const (
	AckDownloaded = "downloaded"
	AckValidated  = "validated"
	AckApplied    = "applied"
	AckFailed     = "failed"
)

// 组件在设备上的推进状态。
const (
	CompPending    = "pending"
	CompDownloaded = "downloaded"
	CompValidated  = "validated"
	CompApplied    = "applied"
	CompFailed     = "failed"
)

// 批次与行动状态。
const (
	BatchPending   = "pending"
	BatchActive    = "active"
	BatchCompleted = "completed"

	CampaignActive    = "active"
	CampaignCompleted = "completed"
)

// Combination 是设备上一组组件版本的组合，例如
// {"firmware": 3, "rf-driver": 2, "billing-app": 5}。
type Combination map[string]int

func (c Combination) Clone() Combination {
	out := make(Combination, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

func (c Combination) Equal(other Combination) bool {
	if len(c) != len(other) {
		return false
	}
	for k, v := range c {
		if ov, ok := other[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

func (c Combination) String() string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := "{"
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=v%d", k, c[k])
	}
	return out + "}"
}

// ComponentVersion 是某个组件的一个已发布版本；撤回只影响后续核对，
// 不会改写设备上已经真实生效的组合。
type ComponentVersion struct {
	Component   string     `json:"component"`
	Version     int        `json:"version"`
	Withdrawn   bool       `json:"withdrawn"`
	ReleasedAt  time.Time  `json:"released_at"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
}

// Condition 表示 “某组件版本 >= MinVersion”。
type Condition struct {
	Component  string `json:"component"`
	MinVersion int    `json:"min_version"`
}

func (c Condition) holds(combo Combination) bool {
	v, ok := combo[c.Component]
	return ok && v >= c.MinVersion
}

// Rule 是一条兼容约束：当 When 成立时，Require 必须同时成立，
// 用于表达组件间的交叉依赖（例如 驱动>=2 要求 固件>=3）。
type Rule struct {
	ID      string    `json:"id"`
	When    Condition `json:"when"`
	Require Condition `json:"require"`
	Note    string    `json:"note,omitempty"`
}

func (r Rule) violatedBy(combo Combination) bool {
	return r.When.holds(combo) && !r.Require.holds(combo)
}

// Ruleset 是一代兼容约束。每次修改生成新的一代并保留历史，
// 与发布决定历史分开查询。
type Ruleset struct {
	Generation int       `json:"generation"`
	Rules      []Rule    `json:"rules"`
	Reason     string    `json:"reason,omitempty"`
	ChangedAt  time.Time `json:"changed_at"`
}

// Campaign 是一次针对“整组组合”的发布/回退行动。
type Campaign struct {
	ID               int          `json:"id"`
	Kind             string       `json:"kind"` // release | rollback | batch_advance | batch_pause | batch_resume
	Target           Combination  `json:"target"`
	DeviceIDs        []string     `json:"device_ids"`
	RulesetGen       int          `json:"ruleset_gen"` // 决策时使用的约束代次
	CreatedAt        time.Time    `json:"created_at"`
	Active           bool         `json:"active"`
	Status           string       `json:"status,omitempty"`
	Paused           bool         `json:"paused,omitempty"`
	CurrentBatchNo   int          `json:"current_batch_no,omitempty"`
	Batches          []BatchState `json:"batches,omitempty"`
	ParentCampaignID int          `json:"parent_campaign_id,omitempty"`
}

type BatchSpec struct {
	Name               string   `json:"name,omitempty"`
	DeviceIDs          []string `json:"device_ids"`
	RequiredComponents []string `json:"required_components,omitempty"`
}

type BatchState struct {
	No                  int      `json:"no"`
	Name                string   `json:"name,omitempty"`
	DeviceIDs           []string `json:"device_ids"`
	OriginalDeviceIDs   []string `json:"original_device_ids,omitempty"`
	RequiredComponents  []string `json:"required_components,omitempty"`
	Status              string   `json:"status"`
	RolledBackDeviceIDs []string `json:"rolled_back_device_ids,omitempty"`
}

// CompProgress 记录设备上单个组件的推进状态。
type CompProgress struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// DevicePlan 是某台设备在当前行动中的推进计划与真实进度。
type DevicePlan struct {
	CampaignID  int                      `json:"campaign_id,omitempty"`
	BatchNo     int                      `json:"batch_no,omitempty"`
	DeviceID    string                   `json:"device_id"`
	Components  map[string]*CompProgress `json:"components"`
	Blocked     []string                 `json:"blocked,omitempty"` // 重新核对后的阻碍原因
	Done        bool                     `json:"done"`
	RolledBack  bool                     `json:"rolled_back,omitempty"`
	CompletedAt *time.Time               `json:"completed_at,omitempty"`
}

// Started 表示设备是否已开始推进（任一组件不再处于 pending）。
func (p *DevicePlan) Started() bool {
	for _, cp := range p.Components {
		if cp.State != CompPending {
			return true
		}
	}
	return false
}

// WaitingOn 列出尚未收尾的组件（部分应用的设备明确等待哪项）。
func (p *DevicePlan) WaitingOn() []string {
	var out []string
	for name, cp := range p.Components {
		if cp.State != CompApplied {
			out = append(out, name+"("+cp.State+")")
		}
	}
	sort.Strings(out)
	return out
}

// Decision 是一条发布/回退决定的历史记录，只追加不修改；
// 后来约束规则的变化记录在 Ruleset 历史里，两者分别可查。
type Decision struct {
	Seq        int                 `json:"seq"`
	Kind       string              `json:"kind"`
	CampaignID int                 `json:"campaign_id"`
	Target     Combination         `json:"target"`
	RulesetGen int                 `json:"ruleset_gen"`
	Accepted   []string            `json:"accepted"`
	Rejected   map[string][]string `json:"rejected,omitempty"` // 设备 -> 阻碍原因
	BatchNo    int                 `json:"batch_no,omitempty"`
	Batches    []BatchSpec         `json:"batches,omitempty"`
	At         time.Time           `json:"at"`
}

// Device 是服务端记录的设备真实状态（心跳自报的当前生效组合）。
type Device struct {
	ID       string      `json:"id"`
	Current  Combination `json:"current"`
	LastSeen *time.Time  `json:"last_seen,omitempty"`
}

// ---- 请求 / 视图 ----

type PlanRequest struct {
	Target Combination `json:"target"`
}

type PlanEntry struct {
	DeviceID string      `json:"device_id"`
	Current  Combination `json:"current"`
	OK       bool        `json:"ok"`
	Reasons  []string    `json:"reasons,omitempty"`
	Note     string      `json:"note,omitempty"`
}

type PlanResponse struct {
	Target     Combination `json:"target"`
	Compatible bool        `json:"compatible"`
	Problems   []string    `json:"problems,omitempty"`
	Devices    []PlanEntry `json:"devices"`
}

type ReleaseRequest struct {
	Target    Combination `json:"target"`
	DeviceIDs []string    `json:"device_ids"`
	Batches   []BatchSpec `json:"batches,omitempty"`
}

type RollbackRequest struct {
	Target     Combination `json:"target"`
	DeviceIDs  []string    `json:"device_ids,omitempty"`
	OnlyFailed bool        `json:"only_failed,omitempty"`
	CampaignID int         `json:"campaign_id,omitempty"`
}

type BatchAdvanceRequest struct {
	CampaignID int `json:"campaign_id,omitempty"`
}

type RegisterRequest struct {
	DeviceID string      `json:"device_id"`
	Current  Combination `json:"current"`
}

type RegisterResponse struct {
	CampaignID int          `json:"campaign_id,omitempty"`
	Target     *Combination `json:"target,omitempty"`
	Blocked    []string     `json:"blocked,omitempty"`
}

type AckRequest struct {
	DeviceID   string `json:"device_id"`
	CampaignID int    `json:"campaign_id"`
	BatchNo    int    `json:"batch_no,omitempty"`
	Component  string `json:"component"`
	Kind       string `json:"kind"` // downloaded | validated | applied | failed
	Reason     string `json:"reason,omitempty"`
}

type DeviceView struct {
	CampaignID  int                      `json:"campaign_id,omitempty"`
	BatchNo     int                      `json:"batch_no,omitempty"`
	BatchName   string                   `json:"batch_name,omitempty"`
	BatchStatus string                   `json:"batch_status,omitempty"`
	DeviceID    string                   `json:"device_id"`
	Current     Combination              `json:"current"`
	Online      bool                     `json:"online"`
	LastSeen    *time.Time               `json:"last_seen,omitempty"`
	InCampaign  bool                     `json:"in_campaign"`
	Target      Combination              `json:"target,omitempty"`
	Components  map[string]*CompProgress `json:"components,omitempty"`
	WaitingOn   []string                 `json:"waiting_on,omitempty"`
	Blocked     []string                 `json:"blocked,omitempty"`
	Failed      []string                 `json:"failed,omitempty"`
	RolledBack  bool                     `json:"rolled_back,omitempty"`
	Done        bool                     `json:"done"`
}

type BatchView struct {
	No                  int          `json:"no"`
	Name                string       `json:"name,omitempty"`
	Status              string       `json:"status"`
	RequiredComponents  []string     `json:"required_components,omitempty"`
	ReadyToAdvance      bool         `json:"ready_to_advance"`
	DeviceIDs           []string     `json:"device_ids"`
	RolledBackDeviceIDs []string     `json:"rolled_back_device_ids,omitempty"`
	Devices             []DeviceView `json:"devices"`
}

type StatusResponse struct {
	Components []ComponentVersion `json:"components"`
	RulesetGen int                `json:"ruleset_gen"`
	Rules      []Rule             `json:"rules"`
	Campaign   *Campaign          `json:"campaign,omitempty"`
	Campaigns  []Campaign         `json:"campaigns,omitempty"`
	Batches    []BatchView        `json:"batches,omitempty"`
	Devices    []DeviceView       `json:"devices"`
}
