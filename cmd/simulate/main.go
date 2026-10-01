package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	ackLost := func(version int, kind string) bool {
		return version == 2 && kind == protocol.AckApplied
	}
	offline := newClient(base, "client-offline", nil)
	applyFail := newClient(base, "client-apply-fail", failV2)
	ackLostClient := newClient(base, "client-ack-lost", nil)
	ackLostClient.SetAckHook(ackLost)
	clients := map[string]*client.Client{
		"client-offline":    offline,
		"client-apply-fail": applyFail,
		"client-ack-lost":   ackLostClient,
	}
	for _, c := range clients {
		c.Start(2 * time.Second)
	}
	defer func() {
		for _, c := range clients {
			c.Stop()
		}
	}()
	time.Sleep(300 * time.Millisecond)

	publish(map[string]interface{}{"app": "demo", "port": 8080, "feature_x": false})
	waitFor(3*time.Second, func() bool { return allActive(clients, 1) })
	showStatus("发布 v1 后，三个客户端均实际生效 v1")

	offline.SetPaused(true)
	publishBatch(map[string]interface{}{"app": "demo", "port": 80, "feature_x": true}, []string{"client-apply-fail", "client-ack-lost"})
	time.Sleep(2500 * time.Millisecond)
	showStatus("v2 首批：应用失败保留 v1，确认丢失实际为 v2，离线端仍等待")
	fmt.Printf("    应用失败端磁盘文件: %s\n", readActive(filepath.Join(base, "client-apply-fail", "active.json")))
	fmt.Printf("    确认丢失端磁盘文件: %s\n\n", readActive(filepath.Join(base, "client-ack-lost", "active.json")))

	fmt.Println("[4] 首批失败后尝试推进离线客户端所在后续批次")
	if promoteBatch(2, []string{"client-offline"}) {
		log.Fatal("应用失败后不应允许后续批次放量")
	}
	showStatus("推进被拒绝，后续批次保持等待")
	fmt.Println()

	fmt.Println("[5] 停止客户端并重启服务端/客户端，按磁盘实际生效版本对账")
	for _, c := range clients {
		c.Stop()
	}
	currentServer = startServer(serverDir)
	offline = newClient(base, "client-offline", nil)
	offline.SetPaused(true)
	applyFail = newClient(base, "client-apply-fail", failV2)
	ackLostClient = newClient(base, "client-ack-lost", nil)
	ackLostClient.SetAckHook(ackLost)
	clients = map[string]*client.Client{
		"client-offline":    offline,
		"client-apply-fail": applyFail,
		"client-ack-lost":   ackLostClient,
	}
	offline.SyncOnce()
	applyFail.SyncOnce()
	ackLostClient.SyncOnce()
	showStatus("重启后心跳对账：下载/验证状态没有被误认为 applied")
	fmt.Println()

	fmt.Println("[6] 显式回退到 v1；不新增版本，也不覆盖 v2 的失败历史")
	rollback(1)
	time.Sleep(1200 * time.Millisecond)
	for _, c := range clients {
		c.Start(500 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	offline.SetPaused(false)
	waitFor(3*time.Second, func() bool {
		return applyFail.ActiveVersion() == 1 && ackLostClient.ActiveVersion() == 1 && offline.ActiveVersion() == 1
	})
	if sendAckOK(protocol.AckRequest{ClientID: "client-ack-lost", Version: 2, Kind: protocol.AckApplied}) {
		log.Fatal("回退后迟到的 v2 applied 不应被接受")
	}
	showStatus("回退对账后：全部实际为 v1，旧 v2 迟到确认不能改成新版本完成")

	fmt.Println("== 演示完成：分批门控 / 三阶段确认 / 失败停批 / 回退保史 / 重启对账 均符合预期 ==")
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
	publishBatch(content, nil)
}

func publishBatch(content map[string]interface{}, batch []string) {
	req := protocol.PublishRequest{Content: content, BatchClientIDs: batch}
	b, _ := json.Marshal(req)
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

func promoteBatch(version int, batch []string) bool {
	b, _ := json.Marshal(map[string]interface{}{"version": version, "batch_client_ids": batch})
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/admin/v1/batches", "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("[rollout] 推进后续批次被拒绝: %s\n", strings.TrimSpace(string(body)))
		return false
	}
	return true
}

func rollback(version int) {
	b, _ := json.Marshal(protocol.RollbackRequest{Version: version})
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/admin/v1/rollback", "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Fatalf("回退失败: %s", body)
	}
	fmt.Printf("[rollback] 当前部署目标已明确设置为 v%d\n", version)
}

func sendAckOK(req protocol.AckRequest) bool {
	b, _ := json.Marshal(req)
	resp, err := currentServer.httpc.Post(currentServer.baseURL+"/api/v1/ack", "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func showStatus(title string) protocol.StatusResponse {
	resp, err := currentServer.httpc.Get(currentServer.baseURL + "/admin/v1/status")
	must(err)
	defer resp.Body.Close()
	var st protocol.StatusResponse
	must(json.NewDecoder(resp.Body).Decode(&st))
	fmt.Println("---", title)
	fmt.Printf("    最新版本: v%d；部署目标: v%d；模式=%s；停止=%v\n",
		st.LatestVersion, st.Deployment.TargetVersion, st.Deployment.Mode, st.Deployment.Stopped)
	for _, c := range st.Clients {
		online := "offline"
		if c.Online {
			online = "online"
		}
		errMsg := ""
		if len(c.Failures) > 0 {
			errMsg = fmt.Sprintf(" failures=%d last=\"%s\"", len(c.Failures), c.LastError)
		}
		fmt.Printf("    %-18s active=v%d target=v%d state=%-10s batch=%-5v %s%s\n",
			c.ClientID, c.ActiveVersion, c.TargetVersion, c.State, c.InCurrentBatch, online, errMsg)
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
