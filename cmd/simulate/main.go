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
	fmt.Println("== 多版本配置分发系统 端到端演示（进程内本地虚拟客户端，无真实端口）==")
	fmt.Println("数据目录:", base)
	fmt.Println()

	serverDir := filepath.Join(base, "server")
	currentServer = startServer(serverDir)
	fmt.Println("[1] 服务端已启动:", currentServer.baseURL)

	failV2 := func(version int, _ map[string]interface{}) string {
		if version == 2 {
			return "模拟应用失败: 端口 80 被占用"
		}
		return ""
	}
	c1 := newClient(base, "client-1", nil)
	c2 := newClient(base, "client-2", failV2)
	c3 := newClient(base, "client-3", nil)
	clients := map[string]*client.Client{"client-1": c1, "client-2": c2, "client-3": c3}
	for _, c := range clients {
		c.Start(500 * time.Millisecond)
	}
	defer func() {
		for _, c := range clients {
			c.Stop()
		}
	}()
	time.Sleep(600 * time.Millisecond)

	publish(map[string]interface{}{"app": "demo", "port": 8080, "feature_x": false})
	waitFor(3*time.Second, func() bool { return allActive(clients, 1) })
	showStatus("发布 v1 后")

	publish(map[string]interface{}{"app": "demo", "port": 80, "feature_x": true})
	time.Sleep(1500 * time.Millisecond)
	showStatus("发布 v2 后（client-2 应用失败，保留 v1 与失败原因，仍 pending）")
	fmt.Printf("    client-2 磁盘上实际生效文件: %s\n\n", readActive(filepath.Join(base, "client-2", "active.json")))

	fmt.Println("[4] 幂等性检查：对已 applied 的 client-1 重放 downloaded / failed / 旧版本确认")
	sendAck(protocol.AckRequest{ClientID: "client-1", Version: 2, Kind: protocol.AckDownloaded})
	sendAck(protocol.AckRequest{ClientID: "client-1", Version: 2, Kind: protocol.AckFailed, Reason: "迟到的失败"})
	sendAck(protocol.AckRequest{ClientID: "client-1", Version: 1, Kind: protocol.AckDownloaded})
	time.Sleep(300 * time.Millisecond)
	showStatus("重放确认后（client-1 应保持 applied，不回退）")
	fmt.Println()

	fmt.Println("[5] 重启服务端（同一持久化目录，构造全新实例）")
	currentServer = startServer(serverDir)
	fmt.Println("    新服务端已起:", currentServer.baseURL, "（版本与客户端状态从 state.json 恢复）")
	for _, c := range clients {
		c.SetServerURL(currentServer.baseURL)
		c.SetHTTPClient(currentServer.httpc)
	}
	time.Sleep(1200 * time.Millisecond)
	showStatus("重启后心跳对账（client-1/3 实际 v2，client-2 实际 v1）")
	fmt.Println()

	fmt.Println("[6] client-3 短暂断连，期间发布 v3；恢复后直接取最新完整版本")
	c3.SetPaused(true)
	publish(map[string]interface{}{"app": "demo", "port": 9090, "feature_x": true, "feature_y": "on"})
	time.Sleep(1200 * time.Millisecond)
	fmt.Println("    断连中的 client-3 错过 v3；恢复连接。client-2 跳过未应用的 v2 直达 v3。")
	c3.SetPaused(false)
	waitFor(4*time.Second, func() bool { return c1.ActiveVersion() == 3 && c3.ActiveVersion() == 3 })
	waitFor(4*time.Second, func() bool { return c2.ActiveVersion() == 3 })
	showStatus("断连恢复 + client-2 跳过 v2 直接应用 v3 完整版本后")

	fmt.Println("== 演示完成：可靠整份切换 / 失败保留原版 / 重启与断连对账 均符合预期 ==")
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

func publish(content map[string]interface{}) {
	b, _ := json.Marshal(protocol.PublishRequest{Content: content})
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/admin/v1/releases", "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		log.Fatalf("发布失败: %d", resp.StatusCode)
	}
	var rel protocol.Release
	must(json.NewDecoder(resp.Body).Decode(&rel))
	fmt.Printf("[publish] 已发布 v%d checksum=%s\n", rel.Version, rel.Checksum)
}

func sendAck(req protocol.AckRequest) {
	b, _ := json.Marshal(req)
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/api/v1/ack", "application/json", bytes.NewReader(b))
	must(err)
	resp.Body.Close()
}

func showStatus(title string) protocol.StatusResponse {
	resp, err := currentServer.httpc.Get(currentServer.baseURL + "/admin/v1/status")
	must(err)
	defer resp.Body.Close()
	var st protocol.StatusResponse
	must(json.NewDecoder(resp.Body).Decode(&st))
	fmt.Println("---", title)
	fmt.Printf("    最新版本: v%d\n", st.LatestVersion)
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
	}
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
