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
		"target": good,
		"batches": []map[string]interface{}{
			{"device_ids": []string{"dev-a"}, "condition": map[string]interface{}{"kind": "all"}},
			{"device_ids": []string{"dev-b", "dev-c"}, "condition": map[string]interface{}{"kind": "count", "count": 2}},
		},
	}, 201).(map[string]interface{})
	campaignID := int(dec["campaign_id"].(float64))
	fmt.Printf("    行动 #%d 已创建：批次0 试运行 [dev-a]，批次1 [dev-b dev-c]（需 2 台全部组件 applied）\n", campaignID)
	fmt.Println()

	fmt.Println("[4] dev-a 仅下载成功不等于组合可用")
	ack("dev-a", campaignID, "firmware", "downloaded", "")
	ack("dev-a", campaignID, "rf-driver", "downloaded", "")
	ack("dev-a", campaignID, "billing-app", "downloaded", "")
	showStatus("全部组件 downloaded：组合仍不可用，等待逐项 applied")
	ack("dev-a", campaignID, "firmware", "applied", "")
	showStatus("dev-a 部分应用：明确等待 rf-driver、billing-app 收尾")
	fmt.Println()

	fmt.Println("[5] 后续批次独立等待：dev-b/dev-c 心跳拿不到目标，确认按迟到拒绝")
	reg := post("/api/v1/combo/register", map[string]interface{}{
		"device_id": "dev-b", "current": oldCombo,
	}, 200).(map[string]interface{})
	if reg["target"] == nil {
		fmt.Println("    dev-b 不在当前批次，心跳未取得目标 ✓")
	}
	if _, ok := tryPost("/api/v1/combo/ack", map[string]interface{}{
		"device_id": "dev-b", "campaign_id": campaignID, "component": "firmware", "kind": "applied",
	}, 400); ok {
		fmt.Println("    dev-b 的提前确认被拒绝（不得越过当前批次）✓")
	}
	fmt.Println()

	fmt.Println("[6] 试运行批未满足确认条件，推进下一批被拒；暂停期间同样被拒")
	if _, ok := tryPost("/admin/v1/combo/batches/advance", map[string]interface{}{}, 409); ok {
		fmt.Println("    条件未满足时推进被拒绝 ✓")
	}
	post("/admin/v1/combo/pause", map[string]interface{}{}, 200)
	if _, ok := tryPost("/admin/v1/combo/batches/advance", map[string]interface{}{}, 409); ok {
		fmt.Println("    暂停推进期间推进被拒绝 ✓")
	}
	post("/admin/v1/combo/resume", map[string]interface{}{}, 200)
	fmt.Println("    已恢复推进")
	fmt.Println()

	fmt.Println("[7] dev-a 的 billing-app 应用失败，失败原因按批独立展示")
	ack("dev-a", campaignID, "billing-app", "failed", "签名校验失败")
	showStatus("批次0 显示 dev-a 的失败原因；批次1 仍待推进、不带批次0 的状态")
	fmt.Println()

	fmt.Println("[8] 只回退失败设备：dev-a 整组回退，dev-b/dev-c 不在范围内继续服务")
	dec = post("/admin/v1/combo/rollback", map[string]interface{}{
		"target": oldCombo, "failed_only": true,
	}, 200).(map[string]interface{})
	rollbackID := int(dec["campaign_id"].(float64))
	fmt.Printf("    回退行动 #%d 目标 %v，仅设备 %v\n", rollbackID, oldCombo, dec["accepted"])
	fmt.Println()

	fmt.Println("[9] 旧行动的迟到确认被拒绝，不得改变已回退的事实")
	if _, ok := tryPost("/api/v1/combo/ack", map[string]interface{}{
		"device_id": "dev-a", "campaign_id": campaignID, "component": "billing-app", "kind": "applied",
	}, 400); ok {
		fmt.Printf("    dev-a 对旧行动 #%d 的迟到 applied 被拒绝 ✓\n", campaignID)
	}
	fmt.Println()

	fmt.Println("[10] 重启服务端，从磁盘恢复每批决定、实际确认与仍待推进的范围")
	startServer(serverDir)
	showStatus("重启后：回退行动仍在进行，dev-a 的待收尾组件保留")
	fmt.Println()

	fmt.Println("[11] dev-a 按回退目标整组收尾，心跳对账完成")
	post("/api/v1/combo/register", map[string]interface{}{"device_id": "dev-a", "current": oldCombo}, 200)
	adv := post("/admin/v1/combo/batches/advance", map[string]interface{}{}, 200).(map[string]interface{})
	fmt.Printf("    回退行动推进完成：%v\n", adv["note"])
	showStatus("dev-a 已回到旧组合；dev-b/dev-c 保持原组合继续服务")
	fmt.Println()

	fmt.Println("[12] 发布决定历史（含分批/推进/暂停）与规则变化分别可查")
	decs := get("/admin/v1/combo/decisions").([]interface{})
	for _, d := range decs {
		m := d.(map[string]interface{})
		line := fmt.Sprintf("    决定 #%v kind=%v target=%v ruleset_gen=%v",
			m["seq"], m["kind"], m["target"], m["ruleset_gen"])
		if b, ok := m["batch"]; ok {
			line += fmt.Sprintf(" batch=%v", b)
		}
		if a, ok := m["accepted"]; ok {
			line += fmt.Sprintf(" accepted=%v", a)
		}
		if n, ok := m["note"]; ok {
			line += fmt.Sprintf(" note=%v", n)
		}
		fmt.Println(line)
	}
	hist := get("/admin/v1/combo/rules/history").([]interface{})
	for _, h := range hist {
		m := h.(map[string]interface{})
		rules, _ := m["rules"].([]interface{})
		fmt.Printf("    规则代次 gen=%v rules=%d 原因=%v\n", m["generation"], len(rules), m["reason"])
	}
	fmt.Println()
	fmt.Println("== 演示完成：分批推进 / 条件确认 / 暂停恢复 / 只回退失败设备 / 批次独立展示 / 迟到确认拒绝 / 重启恢复 ==")
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
		line := fmt.Sprintf("    当前行动 #%v kind=%v target=%v (ruleset_gen=%v) 当前批次=%v",
			camp["id"], camp["kind"], camp["target"], camp["ruleset_gen"], camp["active_batch"])
		if st["paused"] == true {
			line += " [已暂停推进]"
		}
		fmt.Println(line)
	}
	if batches, ok := st["batches"].([]interface{}); ok {
		for _, b := range batches {
			bm := b.(map[string]interface{})
			fmt.Printf("    批次 %v state=%v 条件=%v 条件满足=%v\n",
				bm["index"], bm["state"], bm["condition"], bm["condition_met"])
			for _, d := range bm["devices"].([]interface{}) {
				printDevice(d.(map[string]interface{}))
			}
		}
		return
	}
	for _, d := range st["devices"].([]interface{}) {
		printDevice(d.(map[string]interface{}))
	}
}

func printDevice(m map[string]interface{}) {
	online := "offline"
	if m["online"] == true {
		online = "online"
	}
	line := fmt.Sprintf("      %-6s %-7s current=%v", m["device_id"], online, m["current"])
	if m["in_campaign"] == true {
		line += fmt.Sprintf(" done=%v", m["done"])
		if w, ok := m["waiting_on"].([]interface{}); ok && len(w) > 0 {
			line += fmt.Sprintf(" waiting_on=%v", w)
		}
		if f, ok := m["failed"].([]interface{}); ok && len(f) > 0 {
			line += fmt.Sprintf(" failed=%v", f)
		}
		if b, ok := m["blocked"].([]interface{}); ok && len(b) > 0 {
			line += fmt.Sprintf(" blocked=%v", b)
		}
	}
	fmt.Println(line)
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
