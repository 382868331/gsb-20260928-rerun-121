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

---

# 多组件兼容组合发布（compat）

在单份配置分发之外，仓库还演示了**相互依赖组件的组合式发布**：
只有设备上的组合满足兼容约束，才允许把某个组件推进到新版本。

## 核心语义

- 组合（Combo）是设备上各组件版本的完整快照，兼容校验以组合为单位；
  约束形如 “engine@2 要求 kernel ∈ [2]”，可表达依赖交叉。
- 操作者先 `POST /admin/v1/preview` 看到每台设备可行的组合与阻碍原因
  （离线、约束违反、版本已撤回、上一战役未收尾），再圈选批次发起战役。
- 单个组件的 `downloaded` / `validated` / `applied` 只推进该组件自身状态；
  组合内全部组件 `applied` 才算整体切换完成，单个组件下载成功
  不能宣布组合可用。新战役会继承设备上已 applied 的组件版本。
- 试用期间修改约束（`POST /admin/v1/rules`）或撤回版本
  （`POST /admin/v1/versions/withdraw`）：已应用设备保留真实状态，
  未开始设备重新核对并给出阻碍原因，部分应用设备明确列出等待收尾的组件。
- 回退（`POST /admin/v1/rollback`）必须选择一个仍兼容的完整组合，
  而不是逐项恢复各自旧值；不兼容的回退组合会被拒绝。
- 过期战役的迟到确认被拒绝；状态持久化到磁盘，重启后继续核对。
- 发布决定与规则变化分别记入两条历史：
  `GET /admin/v1/history/decisions` 与 `GET /admin/v1/history/rules`。

## 快速开始

```bash
go run ./cmd/compat-simulate   # 演示：依赖交叉、离线设备、迟到确认、撤回、回退、重启
go run ./cmd/compat-server -addr 127.0.0.1:8081 -data ./compat-runtime
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST/GET | `/admin/v1/versions` | 登记 / 查看组件版本 |
| POST | `/admin/v1/versions/withdraw` | 撤回或恢复版本，触发重新核对 |
| POST/GET | `/admin/v1/rules` | 设置 / 查看兼容约束 |
| POST | `/admin/v1/preview` | 逐台设备评估可行组合与阻碍原因 |
| POST | `/admin/v1/campaigns` | 圈选批次发起发布（目标为完整组合） |
| POST | `/admin/v1/rollback` | 回退到仍兼容的完整组合 |
| GET | `/admin/v1/status` | 各设备实际组合、目标组合、组件状态、等待收尾项 |
| GET | `/admin/v1/history/decisions` | 发布决定历史 |
| GET | `/admin/v1/history/rules` | 规则变化历史 |
| POST | `/api/v1/heartbeat` | 设备心跳：上报实际生效组合对账 |
| POST | `/api/v1/ack` | 单组件确认：downloaded / validated / applied / failed |

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
