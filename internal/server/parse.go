package server

import (
	"fmt"
	"strconv"
	"strings"
)

// fmtSscanf 从 /api/v1/releases/{version} 中解析版本号。
func fmtSscanf(path string, version *int) (int, error) {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return 0, fmt.Errorf("bad path")
	}
	v, err := strconv.Atoi(path[idx+1:])
	if err != nil {
		return 0, err
	}
	*version = v
	return 1, nil
}
