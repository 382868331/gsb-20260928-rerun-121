package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"configdist/internal/client"
	"configdist/internal/protocol"
	"configdist/internal/server"
)

type serverInstance struct {
	baseURL string
	httpc   *http.Client
}

var currentServer *serverInstance

func main() {
	log.SetFlags(log.Ltime)
	base, err := os.MkdirTemp("", "configdist-sim-")
	must(err)
	defer os.RemoveAll(base)
	fmt.Println("== 多版本配置分发系统 端到端演示：分批放量 / 自动停止 / 回退 / 对账 ==")
	fmt.Println("数据目录:", base)
	fmt.Println()

	serverDir := filepath.Join(base, "server")
	currentServer = startServer(serverDir)
	fmt.Println("[1] 服务端已启动:", currentServer.baseURL)

	// 三类异常客户端：应用失败(c2)、确认丢失(c3)、离线(c4)；c1 正常。
	failV2 := func(version int, _ map[string]interface{}) string {
		if version == 2 {
			return "模拟应用失败: 端口 80 被占用"
		}
		return ""
	}
	c1 := newClient(base, "client-1", nil)
	c2 := newClient(base, "client-2", failV2)
	c3 := newClient(base, "client-3", nil)
	c3.SetAckDropper(func(_ int, kind string) bool { return kind == protocol.AckApplied }) // applied 确认丢失
	c4 := newClient(base, "client-4", nil)
	clients := map[string]*client.Client{
		"client-1": c1, "client-2": c2, "client-3": c3, "client-4": c4,
	}
	for _, c := range clients {
		c.Start(400 * time.Millisecond)
	}
	defer func() {
		for _, c := range clients {
			c.Stop()
		}
	}()
	time.Sleep(500 * time.Millisecond)

	fmt.Println("\n[2] 发布 v1（单批全量）；client-3 的 applied 确认丢失，靠心跳对账补记")
	publish(map[string]interface{}{"app": "demo", "port": 8080, "feature_x": false}, nil)
	waitFor(3*time.Second, func() bool { return allActive(clients, 1) })
	time.Sleep(600 * time.Millisecond) // 等 client-3 心跳对账
	showStatus("v1 全量完成（client-3 确认丢失已被对账补记为 applied）")

	fmt.Println("\n[3] 分批发布 v2：批次1=[client-1] 批次2=[client-2] 批次3=[client-3,client-4]")
	c4.SetPaused(true) // client-4 离线
	publish(map[string]interface{}{"app": "demo", "port": 80, "feature_x": true},
		[][]string{{"client-1"}, {"client-2"}, {"client-3", "client-4"}})
	waitFor(3*time.Second, func() bool { return c1.ActiveVersion() == 2 })
	fmt.Println("    批次1 完成，自动放行批次2；client-2 应用失败，后续批次自动停止")
	waitFor(3*time.Second, func() bool {
		st := showStatusSilent()
		return st.Rollout != nil && st.Rollout.Status == protocol.RolloutHalted
	})
	showStatus("批次2 失败后（client-2 保留 v1，client-3/4 目标仍是 v1，放量已停止）")
	fmt.Printf("    client-2 磁盘上实际生效文件: %s\n", readActive(filepath.Join(base, "client-2", "active.json")))

	fmt.Println("\n[4] 发布明确回退目标 v1：client-1 从 v2 回退，失败历史保留")
	rollback(1)
	waitFor(3*time.Second, func() bool { return c1.ActiveVersion() == 1 })
	showStatus("回退后（client-1 已回到 v1；client-2 的 v2 失败历史仍保留）")

	fmt.Println("\n[5] 迟到旧确认：重放 client-1 对 v2 的 applied，不得把回退中的客户端标回新版本")
	sendAck(protocol.AckRequest{ClientID: "client-1", Version: 2, Kind: protocol.AckApplied})
	sendAck(protocol.AckRequest{ClientID: "client-2", Version: 2, Kind: protocol.AckApplied})
	time.Sleep(400 * time.Millisecond)
	showStatus("重放后（所有客户端目标仍为 v1，状态不被迟到确认污染）")

	fmt.Println("\n[6] 重启服务端 + client-4 恢复在线：按实际生效版本对账")
	currentServer = startServer(serverDir)
	fmt.Println("    新服务端已起:", currentServer.baseURL, "（版本/历史/回退目标从 state.json 恢复）")
	for _, c := range clients {
		c.SetServerURL(currentServer.baseURL)
		c.SetHTTPClient(currentServer.httpc)
	}
	c4.SetPaused(false)
	waitFor(3*time.Second, func() bool { return allActive(clients, 1) })
	time.Sleep(600 * time.Millisecond)
	showStatus("重启对账后（全员实际 v1，回退目标保持，失败历史仍在）")

	fmt.Println("\n== 演示完成：分批放量自动停止 / 三阶段确认 / 回退保留历史 / 迟到确认隔离 / 重启对账 ==")
}

func startServer(dir string) *serverInstance {
	store, err := server.NewStore(dir)
	must(err)
	h := server.New(store).Handler()
	return &serverInstance{
		baseURL: "http://configdist.local",
		httpc:   newInProcessClient(h),
	}
}

func newClient(base, id string, hook client.FailHook) *client.Client {
	c, err := client.New(id, currentServer.baseURL, filepath.Join(base, id), hook, nil)
	must(err)
	c.SetHTTPClient(currentServer.httpc)
	return c
}

func publish(content map[string]interface{}, batches [][]string) {
	b, _ := json.Marshal(protocol.PublishRequest{Content: content, Batches: batches})
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/admin/v1/releases", "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		log.Fatalf("发布失败: %d", resp.StatusCode)
	}
	var rel protocol.Release
	must(json.NewDecoder(resp.Body).Decode(&rel))
	fmt.Printf("[publish] 已发布 v%d checksum=%s 批次=%v\n", rel.Version, rel.Checksum, batches)
}

func rollback(version int) {
	b, _ := json.Marshal(protocol.RollbackRequest{Version: version})
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/admin/v1/rollback", "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("回退失败: %d", resp.StatusCode)
	}
	fmt.Printf("[rollback] 回退目标 -> v%d\n", version)
}

func sendAck(req protocol.AckRequest) {
	b, _ := json.Marshal(req)
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/api/v1/ack", "application/json", bytes.NewReader(b))
	must(err)
	resp.Body.Close()
}

func fetchStatus() protocol.StatusResponse {
	resp, err := currentServer.httpc.Get(currentServer.baseURL + "/admin/v1/status")
	must(err)
	defer resp.Body.Close()
	var st protocol.StatusResponse
	must(json.NewDecoder(resp.Body).Decode(&st))
	return st
}

func showStatusSilent() protocol.StatusResponse { return fetchStatus() }

func showStatus(title string) protocol.StatusResponse {
	st := fetchStatus()
	fmt.Println("---", title)
	fmt.Printf("    最新版本: v%d\n", st.LatestVersion)
	if st.Rollout != nil {
		fmt.Printf("    放量: v%d 批次=%v 当前批次=%d 状态=%s %s\n",
			st.Rollout.ReleaseVersion, st.Rollout.Batches, st.Rollout.Current+1,
			st.Rollout.Status, st.Rollout.HaltReason)
	}
	for _, c := range st.Clients {
		online := "offline"
		if c.Online {
			online = "online"
		}
		errMsg := ""
		if c.LastError != "" {
			errMsg = " error=\"" + c.LastError + "\""
		}
		fmt.Printf("    %-9s active=v%d target=v%d state=%-10s %s%s\n",
			c.ClientID, c.ActiveVersion, c.TargetVersion, c.State, online, errMsg)
		for _, r := range st.History[c.ClientID] {
			if r.State == protocol.StateFailed {
				fmt.Printf("      历史: v%d %s reason=\"%s\"\n", r.Version, r.State, r.LastError)
			}
		}
	}
	fmt.Println()
	return st
}

func readActive(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(无)"
	}
	var v map[string]interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func allActive(clients map[string]*client.Client, v int) bool {
	for _, c := range clients {
		if c.ActiveVersion() != v {
			return false
		}
	}
	return true
}

func waitFor(timeout time.Duration, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Fatalf("等待条件超时")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
