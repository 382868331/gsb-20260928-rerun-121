package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"configdist/internal/combo"
)

// inProcessTransport 不监听真实端口，直接把请求交给 http.Handler。
type inProcessTransport struct{ handler http.Handler }

func (t *inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

var httpc *http.Client

const base = "http://combo.local"

func main() {
	log.SetFlags(0)
	dir, err := os.MkdirTemp("", "combosim-")
	must(err)
	defer os.RemoveAll(dir)
	fmt.Println("== 多组件组合发布演示（虚构组件：firmware / rf-driver / billing-app）==")
	fmt.Println("数据目录:", dir)
	fmt.Println()

	serverDir := filepath.Join(dir, "server")
	startServer(serverDir)
	fmt.Println("[1] 发布组件版本，并设置交叉依赖约束")
	for _, v := range []int{1, 2, 3} {
		post("/admin/v1/combo/components/versions", map[string]interface{}{"component": "firmware", "version": v}, 201)
	}
	for _, v := range []int{1, 2} {
		post("/admin/v1/combo/components/versions", map[string]interface{}{"component": "rf-driver", "version": v}, 201)
	}
	for _, v := range []int{4, 5} {
		post("/admin/v1/combo/components/versions", map[string]interface{}{"component": "billing-app", "version": v}, 201)
	}
	post("/admin/v1/combo/rules", map[string]interface{}{
		"reason": "射频驱动 v2 依赖新固件；计费应用 v5 依赖新驱动",
		"rules": []map[string]interface{}{
			{"id": "R1", "when": map[string]interface{}{"component": "rf-driver", "min_version": 2},
				"require": map[string]interface{}{"component": "firmware", "min_version": 3},
				"note":    "rf-driver>=2 需要 firmware>=3"},
			{"id": "R2", "when": map[string]interface{}{"component": "billing-app", "min_version": 5},
				"require": map[string]interface{}{"component": "rf-driver", "min_version": 2},
				"note":    "billing-app>=5 需要 rf-driver>=2"},
		},
	}, 200)
	fmt.Println("    约束: R1 rf-driver>=2 ⇒ firmware>=3；R2 billing-app>=5 ⇒ rf-driver>=2")

	oldCombo := map[string]int{"firmware": 1, "rf-driver": 1, "billing-app": 4}
	for _, id := range []string{"dev-a", "dev-b", "dev-c"} {
		post("/api/v1/combo/register", map[string]interface{}{"device_id": id, "current": oldCombo}, 200)
	}
	fmt.Println("    三台设备已注册，当前组合均为 {firmware=v1 rf-driver=v1 billing-app=v4}")
	fmt.Println()

	fmt.Println("[2] 预演一个违反交叉依赖的目标组合（operator 先看到阻碍原因）")
	bad := map[string]int{"firmware": 1, "rf-driver": 2, "billing-app": 5}
	plan := post("/admin/v1/combo/plan", map[string]interface{}{"target": bad}, 200).(map[string]interface{})
	fmt.Printf("    目标 %v 兼容=%v，问题: %v\n", bad, plan["compatible"], plan["problems"])
	fmt.Println()

	fmt.Println("[3] 预演兼容组合并选择一批设备尝试")
	good := map[string]int{"firmware": 3, "rf-driver": 2, "billing-app": 5}
	plan = post("/admin/v1/combo/plan", map[string]interface{}{"target": good}, 200).(map[string]interface{})
	fmt.Printf("    目标 %v 兼容=%v\n", good, plan["compatible"])
	dec := post("/admin/v1/combo/releases", map[string]interface{}{
		"target": good, "device_ids": []string{"dev-a", "dev-b", "dev-c"},
	}, 201).(map[string]interface{})
	campaignID := int(dec["campaign_id"].(float64))
	fmt.Printf("    行动 #%d 已创建，设备 %v\n", campaignID, dec["accepted"])
	fmt.Println()

	fmt.Println("[4] dev-a 仅下载成功不等于组合可用")
	ack("dev-a", campaignID, "firmware", "downloaded", "")
	ack("dev-a", campaignID, "rf-driver", "downloaded", "")
	ack("dev-a", campaignID, "billing-app", "downloaded", "")
	showStatus("全部组件 downloaded：组合仍不可用，等待逐项 applied")
	ack("dev-a", campaignID, "firmware", "applied", "")
	showStatus("dev-a 部分应用：明确等待 rf-driver、billing-app 收尾")
	fmt.Println()

	fmt.Println("[5] 试用期间撤回 rf-driver v2（dev-b 离线停止心跳、dev-c 未开始）")
	time.Sleep(700 * time.Millisecond)
	post("/admin/v1/combo/components/withdraw", map[string]interface{}{"component": "rf-driver", "version": 2}, 200)
	showStatus("撤回后：未开始设备被重新核对并给出阻碍原因；dev-a 部分应用保留等待项")
	fmt.Println()

	fmt.Println("[6] 回退必须选择仍兼容的整组组合，而不是逐项恢复旧值")
	if _, ok := tryPost("/admin/v1/combo/rollback", map[string]interface{}{
		"target": map[string]int{"firmware": 1, "rf-driver": 2, "billing-app": 4},
	}, 409); ok {
		fmt.Println("    含已撤回版本的回退组合被拒绝 ✓")
	}
	dec = post("/admin/v1/combo/rollback", map[string]interface{}{"target": oldCombo}, 200).(map[string]interface{})
	rollbackID := int(dec["campaign_id"].(float64))
	fmt.Printf("    回退行动 #%d 目标 %v\n", rollbackID, oldCombo)
	fmt.Println()

	fmt.Println("[7] 旧行动的迟到确认被拒绝")
	if _, ok := tryPost("/api/v1/combo/ack", map[string]interface{}{
		"device_id": "dev-c", "campaign_id": campaignID, "component": "firmware", "kind": "applied",
	}, 400); ok {
		fmt.Printf("    dev-c 对旧行动 #%d 的迟到 applied 被拒绝 ✓\n", campaignID)
	}
	fmt.Println()

	fmt.Println("[8] 设备按回退目标整组推进；dev-b 仍离线")
	for _, id := range []string{"dev-a", "dev-c"} {
		post("/api/v1/combo/register", map[string]interface{}{"device_id": id, "current": oldCombo}, 200)
	}
	showStatus("心跳对账：dev-a/dev-c 实际组合已等于回退目标；dev-b 离线等待")
	fmt.Println()

	fmt.Println("[9] 重启服务端，从磁盘恢复后继续核对组合状态")
	startServer(serverDir)
	showStatus("重启后：行动、设备进度、阻碍原因全部保留")
	fmt.Println()

	fmt.Println("[10] 发布决定历史与后来的规则变化分别可查")
	decs := get("/admin/v1/combo/decisions").([]interface{})
	for _, d := range decs {
		m := d.(map[string]interface{})
		fmt.Printf("    决定 #%v kind=%v target=%v ruleset_gen=%v accepted=%v\n",
			m["seq"], m["kind"], m["target"], m["ruleset_gen"], m["accepted"])
	}
	hist := get("/admin/v1/combo/rules/history").([]interface{})
	for _, h := range hist {
		m := h.(map[string]interface{})
		rules, _ := m["rules"].([]interface{})
		fmt.Printf("    规则代次 gen=%v rules=%d 原因=%v\n", m["generation"], len(rules), m["reason"])
	}
	fmt.Println()
	fmt.Println("== 演示完成：组合级门控 / 交叉依赖 / 撤回重核 / 部分应用等待 / 整组回退 / 迟到确认拒绝 / 重启恢复 ==")
}

func startServer(dir string) {
	store, err := combo.NewStore(dir)
	must(err)
	srv := combo.NewServer(store)
	srv.SetOfflineWindow(600 * time.Millisecond)
	httpc = &http.Client{Transport: &inProcessTransport{handler: srv.Handler()}}
}

func ack(device string, campaignID int, component, kind, reason string) {
	post("/api/v1/combo/ack", map[string]interface{}{
		"device_id": device, "campaign_id": campaignID, "component": component, "kind": kind, "reason": reason,
	}, 200)
}

func showStatus(title string) {
	st := get("/admin/v1/combo/status").(map[string]interface{})
	fmt.Println("---", title)
	if camp, ok := st["campaign"].(map[string]interface{}); ok {
		fmt.Printf("    当前行动 #%v kind=%v target=%v (ruleset_gen=%v)\n",
			camp["id"], camp["kind"], camp["target"], camp["ruleset_gen"])
	}
	for _, d := range st["devices"].([]interface{}) {
		m := d.(map[string]interface{})
		online := "offline"
		if m["online"] == true {
			online = "online"
		}
		line := fmt.Sprintf("    %-6s %-7s current=%v", m["device_id"], online, m["current"])
		if m["in_campaign"] == true {
			line += fmt.Sprintf(" done=%v", m["done"])
			if w, ok := m["waiting_on"].([]interface{}); ok && len(w) > 0 {
				line += fmt.Sprintf(" waiting_on=%v", w)
			}
			if b, ok := m["blocked"].([]interface{}); ok && len(b) > 0 {
				line += fmt.Sprintf(" blocked=%v", b)
			}
		}
		fmt.Println(line)
	}
}

func post(path string, body interface{}, want int) interface{} {
	v, ok := tryPost(path, body, want)
	if !ok {
		os.Exit(1)
	}
	return v
}

func tryPost(path string, body interface{}, want int) (interface{}, bool) {
	b, _ := json.Marshal(body)
	resp, err := httpc.Post(base+path, "application/json", bytes.NewReader(b))
	must(err)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		if want >= 400 && resp.StatusCode == want {
			return nil, false
		}
		fmt.Printf("    POST %s -> %d %s\n", path, resp.StatusCode, string(data))
		return nil, resp.StatusCode == want
	}
	var v interface{}
	_ = json.Unmarshal(data, &v)
	return v, true
}

func get(path string) interface{} {
	resp, err := httpc.Get(base + path)
	must(err)
	defer resp.Body.Close()
	var v interface{}
	must(json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
