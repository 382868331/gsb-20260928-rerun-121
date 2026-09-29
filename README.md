# 多版本配置分发系统（configdist）

用 Go 实现的服务端 + 多个本地虚拟客户端，演示**整份配置的可靠切换**
（不是文件同步工具）。

## 核心语义

- 每个版本是一份**完整配置快照**（非增量）。客户端缺少中间版本时，
  直接拉取最新完整版本即可生效。
- 客户端切换是**原子**的：配置先写入暂存文件并校验，再 `rename` 覆盖
  当前生效配置；应用失败时原配置保持不变。
- 服务端严格区分两种确认：
  - `downloaded`：客户端仅完成下载，**不代表应用成功**；
  - `applied`：客户端确认新配置已实际生效。
- 每个客户端对每个版本的状态**单调推进**：
  `pending → downloaded → applied`，或 `→ failed`。
  重复确认、乱序确认都不会把状态回退（例如已 `applied` 后再收到
  `downloaded` 或 `failed` 不会改变结果）。
- 应用失败会保留失败原因，且该客户端对更新版本仍处于 `pending`
  待处理状态，下次同步继续。
- 服务端把版本与客户端状态持久化到磁盘；服务端/客户端重启或短暂
  断连后，客户端心跳上报**实际生效版本**，服务端据此对账恢复。

## 快速开始

```bash
go run ./cmd/simulate
```

该演示会：启动服务端与 3 个本地虚拟客户端 → 发布 v1/v2 →
让一个客户端模拟应用失败并保留原版 → 重启服务端 → 断连/恢复后
对账 → 发布 v3，失败客户端直接取最新全量版本恢复一致。

也可以手动运行：

```bash
go run ./cmd/server  -addr 127.0.0.1:8080 -data ./runtime-data
go run ./cmd/client  -server http://127.0.0.1:8080 -id client-1 -data ./c1
curl -X POST localhost:8080/admin/v1/releases -d '{"content":{...}}'
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/admin/v1/releases` | 发布一份完整配置，版本号自增 |
| GET | `/admin/v1/status` | 查看各客户端实际版本、目标版本、状态与失败原因 |
| GET | `/api/v1/releases/latest` | 获取最新完整快照 |
| GET | `/api/v1/releases/{version}` | 下载指定版本完整快照（可跳过中间版本） |
| POST | `/api/v1/register` | 注册/心跳：上报 `active_version` 对账，返回最新版本 |
| POST | `/api/v1/ack` | 确认：`downloaded` / `applied` / `failed`（可带 `reason`） |

`downloaded` 与 `applied` 是两个独立确认；服务端只有在收到 `applied`
后才把客户端的实际版本前移。状态机单调推进，重复确认幂等。
