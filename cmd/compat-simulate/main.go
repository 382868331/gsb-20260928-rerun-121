// compat-simulate 演示多组件兼容组合的发布编排：
// 依赖交叉、离线设备、迟到确认、试用期间撤回版本、整体组合回退、
// 重启后继续核对，以及发布决定与规则变化两条独立历史。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"configdist/internal/compat"
)

var store *compat.Store

func main() {
	dataDir := flag.String("data", "./compat-runtime", "持久化目录")
	flag.Parse()
	if err := os.RemoveAll(*dataDir); err != nil {
		log.Fatal(err)
	}
	var err error
	store, err = compat.NewStore(*dataDir)
	if err != nil {
		log.Fatal(err)
	}

	step("1. 登记虚构组件版本（kernel / engine / ui）")
	for _, v := range [][2]interface{}{{"kernel", 1}, {"kernel", 2}, {"engine", 1}, {"engine", 2}, {"ui", 1}, {"ui", 2}, {"ui", 3}} {
		must(store.RegisterVersion(v[0].(string), v[1].(int)))
	}

	step("2. 设置依赖交叉的兼容约束")
	must(store.SetRule(compat.Rule{ID: "engine-needs-kernel", Component: "engine", Version: 2,
		Requires: map[string][]int{"kernel": {2}}, Note: "engine@2 依赖新内核"}))
	must(store.SetRule(compat.Rule{ID: "ui-needs-engine", Component: "ui", Version: 3,
		Requires: map[string][]int{"engine": {2}}, Note: "ui@3 依赖新引擎"}))

	step("3. 设备心跳：dev-1/dev-3 在线，dev-2 离线")
	base := compat.Combo{"kernel": 1, "engine": 1, "ui": 2}
	must(store.Heartbeat("dev-1", base, true))
	must(store.Heartbeat("dev-2", base, false))
	must(store.Heartbeat("dev-3", base, true))

	target := compat.Combo{"kernel": 2, "engine": 2, "ui": 3}
	step("4. 发布前 Preview：逐台设备看可行组合与阻碍原因")
	preview(target, "dev-1", "dev-2", "dev-3")

	step("5. 操作者圈选批次 [dev-1 dev-2] 发起战役 #1")
	camp, err := store.StartCampaign("rollout", target, []string{"dev-1", "dev-2"}, "首批试用")
	must(err)
	fmt.Printf("  战役 #%d 目标 %s\n", camp.ID, camp.Target.Key())

	step("6. dev-1 仅下载 ui —— 单个组件下载不代表组合可用")
	must(store.Ack(compat.AckRequest{DeviceID: "dev-1", CampaignID: 1, Component: "ui", Kind: "downloaded"}))
	printDevice("dev-1")

	step("7. dev-1 应用 kernel/engine —— 部分应用，明确等待 ui@3 收尾")
	must(store.Ack(compat.AckRequest{DeviceID: "dev-1", CampaignID: 1, Component: "kernel", Kind: "applied"}))
	must(store.Ack(compat.AckRequest{DeviceID: "dev-1", CampaignID: 1, Component: "engine", Kind: "applied"}))
	printDevice("dev-1")

	step("8. 试用期间撤回 ui@3：未开始的 dev-2 重新核对，dev-1 保留真实状态")
	must(store.WithdrawVersion("ui", 3, true))
	printDevice("dev-1")
	printDevice("dev-2")

	step("9. 登记替代版本 ui@4 并追加约束，重新 Preview 后发起战役 #2")
	must(store.RegisterVersion("ui", 4))
	must(store.SetRule(compat.Rule{ID: "ui4-needs-engine", Component: "ui", Version: 4,
		Requires: map[string][]int{"engine": {2}}}))
	target2 := compat.Combo{"kernel": 2, "engine": 2, "ui": 4}
	preview(target2, "dev-1", "dev-2")
	camp2, err := store.StartCampaign("rollout", target2, []string{"dev-1", "dev-2"}, "ui@3 撤回后的替代组合")
	must(err)

	step("10. 迟到确认：dev-1 对已过期战役 #1 的 applied 被拒绝")
	err = store.Ack(compat.AckRequest{DeviceID: "dev-1", CampaignID: 1, Component: "ui", Kind: "applied"})
	fmt.Printf("  拒绝: %v\n", err)

	step("11. dev-1 应用 ui@4 —— 组合内全部组件 applied，整体切换完成")
	must(store.Ack(compat.AckRequest{DeviceID: "dev-1", CampaignID: camp2.ID, Component: "ui", Kind: "applied"}))
	printDevice("dev-1")

	step("12. 回退必须选择仍兼容的组合，而不是逐项恢复旧值")
	bad := compat.Combo{"kernel": 1, "engine": 2, "ui": 2}
	if _, err := store.StartCampaign("rollback", bad, []string{"dev-1"}, ""); err != nil {
		fmt.Printf("  拒绝不兼容回退 %s: %v\n", bad.Key(), err)
	}
	good := compat.Combo{"kernel": 1, "engine": 1, "ui": 2}
	camp3, err := store.StartCampaign("rollback", good, []string{"dev-1"}, "回退到旧但兼容的组合")
	must(err)
	fmt.Printf("  战役 #%d rollback 目标 %s\n", camp3.ID, camp3.Target.Key())

	step("13. 模拟重启：从磁盘恢复，继续核对组合状态")
	store, err = compat.NewStore(*dataDir)
	must(err)
	printDevice("dev-1")
	printDevice("dev-2")

	step("14. 两条历史分别可查：发布决定 vs 规则变化")
	fmt.Println("  -- 发布决定 --")
	for _, h := range store.Decisions() {
		fmt.Printf("  [%s] %s: %s\n", h.At.Format("15:04:05"), h.Kind, h.Detail)
	}
	fmt.Println("  -- 规则变化 --")
	for _, h := range store.RuleEvents() {
		fmt.Printf("  [%s] %s: %s\n", h.At.Format("15:04:05"), h.Kind, h.Detail)
	}
	fmt.Println("\n演示完成。")
}

func step(msg string) { fmt.Printf("\n=== %s ===\n", msg) }

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func preview(target compat.Combo, ids ...string) {
	views, err := store.Preview(target, ids)
	must(err)
	for _, v := range views {
		if v.Feasible {
			fmt.Printf("  %s: 可行 %s\n", v.DeviceID, target.Key())
		} else {
			fmt.Printf("  %s: 阻碍 -> %s\n", v.DeviceID, strings.Join(v.Reasons, "; "))
		}
	}
}

func printDevice(id string) {
	devices, _ := store.Status()
	d := devices[id]
	fmt.Printf("  %s: current=%s target=%s complete=%v online=%v\n",
		id, d.Current.Key(), comboKey(d.Target), d.Complete, d.Online)
	if len(d.WaitingFor) > 0 {
		fmt.Printf("    等待收尾: %s\n", strings.Join(d.WaitingFor, ", "))
	}
	if len(d.Blocked) > 0 {
		fmt.Printf("    阻碍: %s\n", strings.Join(d.Blocked, "; "))
	}
	if d.LastError != "" {
		fmt.Printf("    错误: %s\n", d.LastError)
	}
}

func comboKey(c compat.Combo) string {
	if c == nil {
		return "-"
	}
	return c.Key()
}
