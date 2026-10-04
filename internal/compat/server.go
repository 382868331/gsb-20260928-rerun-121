package compat

import (
	"encoding/json"
	"log"
	"net/http"
)

// Server 提供组合发布编排的 HTTP API。
type Server struct {
	store *Store
}

func NewServer(store *Store) *Server { return &Server{store: store} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/admin/v1/versions", s.handleVersions)
	mux.HandleFunc("/admin/v1/versions/withdraw", s.handleWithdraw)
	mux.HandleFunc("/admin/v1/rules", s.handleRules)
	mux.HandleFunc("/admin/v1/preview", s.handlePreview)
	mux.HandleFunc("/admin/v1/campaigns", s.handleCampaigns)
	mux.HandleFunc("/admin/v1/rollback", s.handleRollback)
	mux.HandleFunc("/admin/v1/status", s.handleStatus)
	mux.HandleFunc("/admin/v1/history/decisions", s.handleDecisions)
	mux.HandleFunc("/admin/v1/history/rules", s.handleRuleEvents)
	mux.HandleFunc("/api/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/api/v1/ack", s.handleAck)
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
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return false
	}
	return true
}

// POST /admin/v1/versions 登记组件版本；GET 返回全部版本。
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"versions": s.store.Versions()})
	case http.MethodPost:
		var req struct {
			Component string `json:"component"`
			Version   int    `json:"version"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := s.store.RegisterVersion(req.Component, req.Version); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// POST /admin/v1/versions/withdraw 撤回或恢复版本。
func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		Component string `json:"component"`
		Version   int    `json:"version"`
		Withdrawn bool   `json:"withdrawn"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.WithdrawVersion(req.Component, req.Version, req.Withdrawn); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// POST /admin/v1/rules 新增/替换约束；GET 返回当前约束。
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"rules": s.store.Rules()})
	case http.MethodPost:
		var rule Rule
		if !decode(w, r, &rule) {
			return
		}
		if err := s.store.SetRule(rule); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rule_set"})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// POST /admin/v1/preview 发布前评估每台设备可行性与阻碍原因。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		Target    Combo    `json:"target"`
		DeviceIDs []string `json:"device_ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	views, err := s.store.Preview(req.Target, req.DeviceIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"devices": views})
}

// POST /admin/v1/campaigns 圈选批次发起发布。
func (s *Server) handleCampaigns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		Target    Combo    `json:"target"`
		DeviceIDs []string `json:"device_ids"`
		Note      string   `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	camp, err := s.store.StartCampaign("rollout", req.Target, req.DeviceIDs, req.Note)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	log.Printf("[compat] 战役 #%d rollout 目标 %s 设备 %v", camp.ID, camp.Target.Key(), camp.DeviceIDs)
	writeJSON(w, http.StatusCreated, camp)
}

// POST /admin/v1/rollback 回退到仍兼容的完整组合（非逐项恢复旧值）。
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		Target    Combo    `json:"target"`
		DeviceIDs []string `json:"device_ids"`
		Note      string   `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	camp, err := s.store.StartCampaign("rollback", req.Target, req.DeviceIDs, req.Note)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	log.Printf("[compat] 战役 #%d rollback 目标 %s 设备 %v", camp.ID, camp.Target.Key(), camp.DeviceIDs)
	writeJSON(w, http.StatusCreated, camp)
}

// GET /admin/v1/status 各设备实际组合、目标组合、组件状态与等待收尾项。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	devices, campaigns := s.store.Status()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"devices": devices, "campaigns": campaigns,
	})
}

// GET /admin/v1/history/decisions 发布决定历史。
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"decisions": s.store.Decisions()})
}

// GET /admin/v1/history/rules 规则变化历史。
func (s *Server) handleRuleEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"rule_events": s.store.RuleEvents()})
}

// POST /api/v1/heartbeat 设备心跳：上报实际生效组合对账。
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
		Current  Combo  `json:"current"`
		Online   bool   `json:"online"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.DeviceID == "" {
		writeErr(w, http.StatusBadRequest, "device_id 必填")
		return
	}
	if err := s.store.Heartbeat(req.DeviceID, req.Current, req.Online); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/v1/ack 单组件确认（downloaded/validated/applied/failed）。
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		DeviceID   string `json:"device_id"`
		CampaignID int    `json:"campaign_id"`
		Component  string `json:"component"`
		Kind       string `json:"kind"`
		Reason     string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.store.Ack(AckRequest{
		DeviceID: req.DeviceID, CampaignID: req.CampaignID,
		Component: req.Component, Kind: req.Kind, Reason: req.Reason,
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}
