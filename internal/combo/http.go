package combo

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Server 提供组合发布的 HTTP API。
type Server struct {
	store *Store

	mu      sync.Mutex
	online  map[string]time.Time
	offline time.Duration
}

func NewServer(store *Store) *Server {
	return &Server{
		store:   store,
		online:  map[string]time.Time{},
		offline: 10 * time.Second,
	}
}

// SetOfflineWindow 调整心跳在线判定窗口（演示/测试用）。
func (s *Server) SetOfflineWindow(d time.Duration) { s.offline = d }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/admin/v1/combo/components/versions", s.handlePublishVersion)
	mux.HandleFunc("/admin/v1/combo/components/withdraw", s.handleWithdraw)
	mux.HandleFunc("/admin/v1/combo/rules", s.handleRules)
	mux.HandleFunc("/admin/v1/combo/rules/history", s.handleRulesHistory)
	mux.HandleFunc("/admin/v1/combo/plan", s.handlePlan)
	mux.HandleFunc("/admin/v1/combo/releases", s.handleRelease)
	mux.HandleFunc("/admin/v1/combo/rollback", s.handleRollback)
	mux.HandleFunc("/admin/v1/combo/status", s.handleStatus)
	mux.HandleFunc("/admin/v1/combo/decisions", s.handleDecisions)
	mux.HandleFunc("/api/v1/combo/register", s.handleRegister)
	mux.HandleFunc("/api/v1/combo/ack", s.handleAck)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return false
	}
	return true
}

// POST /admin/v1/combo/components/versions 发布组件版本。
func (s *Server) handlePublishVersion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Component string `json:"component"`
		Version   int    `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.PublishVersion(req.Component, req.Version); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "published"})
}

// POST /admin/v1/combo/components/withdraw 撤回组件版本。
func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Component string `json:"component"`
		Version   int    `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.WithdrawVersion(req.Component, req.Version); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "withdrawn"})
}

// GET 查看当前规则；POST 以新一代规则替换（旧代次入历史）。
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		hist := s.store.RulesetHistory()
		writeJSON(w, http.StatusOK, hist[len(hist)-1])
		return
	}
	var req struct {
		Rules  []Rule `json:"rules"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.store.SetRules(req.Rules, req.Reason))
}

// GET /admin/v1/combo/rules/history 规则代次历史（与发布决定分开可查）。
func (s *Server) handleRulesHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.RulesetHistory())
}

// POST /admin/v1/combo/plan 预演：逐设备展示能否采用目标组合与阻碍原因。
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.store.Plan(req.Target))
}

// POST /admin/v1/combo/releases 选择一批设备尝试推进到目标组合。
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req ReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	dec, err := s.store.Release(req)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, dec)
}

// POST /admin/v1/combo/rollback 回退到仍兼容的整组组合。
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req RollbackRequest
	if !decode(w, r, &req) {
		return
	}
	dec, err := s.store.Rollback(req)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dec)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot(s.isOnline))
}

// GET /admin/v1/combo/decisions 发布/回退决定历史。
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Decisions())
}

// POST /api/v1/combo/register 设备心跳：上报实际生效组合并取回目标。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.store.Register(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.markOnline(req.DeviceID)
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/v1/combo/ack 组件级确认（downloaded/validated/applied/failed）。
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	var req AckRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.Ack(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.markOnline(req.DeviceID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (s *Server) markOnline(id string) {
	s.mu.Lock()
	s.online[id] = time.Now()
	s.mu.Unlock()
}

func (s *Server) isOnline(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.online[id]
	return ok && time.Since(t) < s.offline
}
