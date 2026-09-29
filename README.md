# 多版本配置分发系统（Go）

面向应用的**整份配置可靠切换**系统，不是文件同步工具。一个服务端管理若干个完整配置版本（快照），多个本地虚拟客户端周期性协调到“期望版本”，并回报真实状态。

## 核心语义

- **整份快照切换**：每次发布都是一份完整配置；客户端错过中间版本也直接拉取最新完整版本，不做增量。
- **下载不等于成功**：`downloaded` 只表示已取到快照，服务端绝不据此标记成功；只有 `applied` 才是成功。
- **失败保留原版**：应用失败时客户端原子状态不变，继续运行原版本；服务端保留失败原因与待处理版本，客户端稍后自动重试。
- **确认单调、幂等**：更旧版本的 `applied` 确认被忽略，重复确认不回退、不重新产生待处理状态。
- **重启 / 断开恢复**：服务端与客户端状态均持久化（原子写盘）。`POST /reconcile` 由服务端回连各客户端 `GET /actual`，以**真实在用版本**纠正记录。

## 结构

```
cmd/server      服务端入口
cmd/client      本地虚拟客户端入口
internal/protocol  HTTP 请求/响应与状态模型
internal/store     带锁、原子落盘的 JSON 状态库
internal/server    发布/清单/快照/确认/对账/状态接口
internal/client    拉取-暂存-应用-汇报循环，故障注入与状态持久化
internal/memhttp   按主机名路由的内存 HTTP 传输（无端口测试）
internal/disttest  端到端语义测试
```

## HTTP 接口（服务端）

| 方法/路径 | 说明 |
| --- | --- |
| `POST /publish` | 发布完整版本 `{id, sequence, content}`，sequence 必须递增 |
| `POST /register` | 客户端注册 `{id, addr}`，重启后重复注册安全 |
| `GET /manifest` | 返回当前期望版本 id/seq |
| `GET /config?id=` | 取某版本完整快照；省略 id 取期望版本 |
| `POST /report` | 客户端汇报 `downloaded` / `applied` / `failed`（含原因） |
| `POST /reconcile` | 回连各客户端 `/actual`，按真实版本纠正服务端记录 |
| `GET /status` | 查看期望版本与各客户端状态（含失败原因、待处理版本） |

## 运行

```bash
go test ./... -race

go run ./cmd/server -addr 127.0.0.1:18080 -data data/server/state.json
go run ./cmd/client -id c1 -addr 127.0.0.1:19001 -server http://127.0.0.1:18080 -data data/c1
go run ./cmd/client -id c2 -addr 127.0.0.1:19002 -server http://127.0.0.1:18080 -data data/c2

curl -sX POST localhost:18080/publish -d '{"id":"v1","sequence":1,"content":{"x":1}}'
curl -s localhost:18080/status
curl -sX POST localhost:18080/reconcile
```

也可运行 `examples/demo.sh`（需要可绑定本地回环端口）观看发布、失败注入说明与对账。
