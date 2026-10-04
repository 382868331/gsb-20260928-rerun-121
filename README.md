# 多版本配置分发系统（configdist）

用 Go 实现的服务端 + 多个本地虚拟客户端，演示**整份配置的可靠切换**
（不是文件同步工具）。

## 核心语义

- 每个版本是一份**完整配置快照**（非增量）。客户端缺少中间版本时，
  直接拉取最新完整版本即可生效。
- 客户端切换是**原子**的：配置先写入暂存文件并校验，再 `rename` 覆盖
  当前生效配置；应用失败时原配置保持不变。
- 服务端严格区分三种确认：
	- `downloaded`：客户端仅完成下载，**不代表应用成功**；
	- `validated`：客户端已完成校验和/业务校验，**仍未真正切换**；
	- `applied`：客户端确认新配置已实际生效。
- 每个客户端对当前部署目标的状态按
	`pending → downloaded → validated → applied` 推进，也可记录为
	`failed`；下载和验证成功都不能作为推广依据。
- 发布新版本时可指定首批客户端。只有当前批次中的客户端能收到目标快照；
	某一批出现真实应用失败后会自动标记 `stopped`，后续批次推进被拒绝。
- 应用失败时客户端原子切换不会发生，继续保留旧版配置；服务端追加失败历史，
	不会用后续目标或回退操作覆盖原失败记录。
- 回退不创建新版本，而是把现有旧版本显式设置为部署目标。回退期间收到的
	旧版本迟到确认会因目标版本不匹配而被拒绝。
- 服务端把版本与客户端状态持久化到磁盘；服务端/客户端重启或短暂
	断连后，客户端心跳上报**实际生效版本**，服务端据此对账恢复。

## 快速开始

```bash
go run ./cmd/simulate
```

该演示会：启动服务端与 3 个本地虚拟客户端（离线、应用失败、应用确认丢失）
→ 全量发布 v1 → 首批发布 v2 → 应用失败自动冻结后续批次 → 重启后按磁盘实际
版本对账 → 显式回退到 v1 → 拒绝迟到的 v2 应用确认。

也可以手动运行：

```bash
go run ./cmd/server  -addr 127.0.0.1:8080 -data ./runtime-data
go run ./cmd/client  -server http://127.0.0.1:8080 -id client-1 -data ./c1
curl -X POST localhost:8080/admin/v1/releases -d '{"content":{...}}'
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/admin/v1/releases` | 发布完整配置；可用 `batch_client_ids` 指定首批 |
| POST | `/admin/v1/batches` | 推进后续批次；当前批次失败后返回冲突错误 |
| POST | `/admin/v1/rollback` | 把已发布的旧版本设置为明确回退目标 |
| GET | `/admin/v1/status` | 查看各客户端实际版本、目标版本、状态与失败原因 |
| GET | `/api/v1/releases/latest` | 获取最新完整快照 |
| GET | `/api/v1/releases/{version}` | 下载指定版本完整快照（可跳过中间版本） |
| POST | `/api/v1/register` | 注册/心跳：上报 `active_version` 对账，返回最新版本 |
| POST | `/api/v1/ack` | 确认：`downloaded` / `validated` / `applied` / `failed` |

首批发布示例：

```json
{
  "content": {"app": "demo", "feature_x": true},
  "batch_client_ids": ["canary-1", "canary-2"]
}
```

`downloaded`、`validated` 与 `applied` 是三个独立确认；服务端只有在收到
`applied`，或重启后心跳确认实际生效版本已等于目标时，才把客户端标记为完成。
不在当前批次中的客户端状态为 `waiting`，心跳不会取得新快照。

---

# 多组件组合发布（combo）

`internal/combo` + `cmd/combosim` 演示**相互依赖的多组件按整组组合推进**，
只有设备上的组合满足兼容约束，才允许把组件推进到新版本。

## 核心语义

- 设备状态是一整组**组合**（如 `{firmware=v3, rf-driver=v2, billing-app=v5}`），
  不是各组件独立的版本列表。
- 兼容约束是**交叉依赖规则**：`When` 成立时 `Require` 必须同时成立
  （例如 `rf-driver>=2 ⇒ firmware>=3`）。每次修改规则生成新的**代次**，
  全部代次保留在规则历史中。
- 操作者先 `plan` 预演：逐设备看到能否采用目标组合与**阻碍原因**，
  再选择一批设备 `release` 尝试。
- 组件级确认严格区分 `downloaded / validated / applied`：
  **单个组件下载成功不代表整组可用**；只有全部组件 `applied`
  （或重启后心跳自报组合已等于目标），设备才算完成。
- 试用期间**修改约束或撤回版本**时：
  - 已完成设备**保留真实状态**，不被改写；
  - 尚未开始的设备按新规则/版本清单**重新核对**，不满足则给出阻碍原因；
  - 部分应用的设备不清除进度，`waiting_on` 明确列出**等待哪项收尾**。
- **回退**是选择一个仍然兼容的整组组合作为目标，而不是逐项恢复各自旧值；
  含已撤回版本的回退组合会被拒绝。
- 确认携带行动 ID，旧行动的**迟到确认**直接拒绝。
- 全部状态持久化到磁盘，重启后继续核对组合状态；
  **发布决定历史**与**后来的规则变化**分别可查
  （`/admin/v1/combo/decisions` 与 `/admin/v1/combo/rules/history`）。

## 运行演示

```bash
go run ./cmd/combosim
```

演示使用虚构组件 `firmware / rf-driver / billing-app`，依次展示：
交叉依赖阻碍预演 → 选批发布 → 下载成功≠组合可用 → 部分应用等待收尾 →
撤回版本触发重核 → 含撤回版本的回退被拒 → 整组回退 → 迟到确认被拒 →
重启恢复 → 决定历史与规则历史分别查询。

## HTTP API（combo）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/admin/v1/combo/components/versions` | 发布组件版本 |
| POST | `/admin/v1/combo/components/withdraw` | 撤回组件版本（触发未开始设备重核） |
| GET/POST | `/admin/v1/combo/rules` | 查看 / 以新代次替换兼容约束 |
| GET | `/admin/v1/combo/rules/history` | 规则代次历史 |
| POST | `/admin/v1/combo/plan` | 预演：逐设备可行性与阻碍原因 |
| POST | `/admin/v1/combo/releases` | 选择一批设备尝试推进到目标组合 |
| POST | `/admin/v1/combo/rollback` | 回退到仍兼容的整组组合 |
| GET | `/admin/v1/combo/status` | 各设备实际组合、进度、等待项、阻碍原因 |
| GET | `/admin/v1/combo/decisions` | 发布/回退决定历史（只追加） |
| POST | `/api/v1/combo/register` | 设备心跳：上报实际组合，取回目标 |
| POST | `/api/v1/combo/ack` | 组件级确认：`downloaded/validated/applied/failed` |
