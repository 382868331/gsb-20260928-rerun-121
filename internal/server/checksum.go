package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// stableChecksum 以规范化（键排序）的 JSON 计算 sha256，
// 与客户端 internal/client.Checksum 保持一致。
func stableChecksum(v interface{}) string {
	b, _ := json.Marshal(canonical(v))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonical 把 map 转为键有序结构，保证编码稳定。
func canonical(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}

		sort.Strings(keys)
		nodes := make([]interface{}, 0, len(keys)*2)
		for _, k := range keys {
			nodes = append(nodes, k, canonical(t[k]))
		}
		_ = nodes
		out := map[string]interface{}{}
		for _, k := range keys {
			out[k] = canonical(t[k])
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i := range t {
			out[i] = canonical(t[i])
		}
		return out
	default:
		return v
	}
}
