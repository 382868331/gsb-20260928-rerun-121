package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"configdist/internal/protocol"
)

// Server 提供配置分发的 HTTP API。
type Server struct {
	store *Store

	mu      sync.Mutex
	online  map[string]time.Time
	offline time.Duration
}

func New(store *Store) *Server {
	return &Server{
		store:   store,
		online:  map[string]time.Time{},
		offline: 10 * time.Second,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/admin/v1/releases", s.handlePublish)
	mux.HandleFunc("/admin/v1/batches", s.handlePromoteBatch)
	mux.HandleFunc("/admin/v1/rollback", s.handleRollback)
	mux.HandleFunc("/api/v1/releases/latest", s.handleLatest)
	mux.HandleFunc("/api/v1/releases/", s.handleGetRelease)
	mux.HandleFunc("/api/v1/register", s.handleRegister)
	mux.HandleFunc("/api/v1/ack", s.handleAck)
	mux.HandleFunc("/admin/v1/status", s.handleStatus)
	return logMiddleware(mux)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /admin/v1/releases 发布完整配置新版本。
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req protocol.PublishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return
	}
	if req.Content == nil {
		writeErr(w, http.StatusBadRequest, "content 必填（整份配置）")
		return
	}
	rel, err := s.store.Publish(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[server] 发布新版本 v%d checksum=%s", rel.Version, rel.Checksum)
	writeJSON(w, http.StatusCreated, rel)
}

func (s *Server) handlePromoteBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		Version        int      `json:"version"`
		BatchClientIDs []string `json:"batch_client_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return
	}
	if err := s.store.PromoteBatch(req.Version, req.BatchClientIDs); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "promoted"})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req protocol.RollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return
	}
	rel, err := s.store.Rollback(req.Version)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.store.Latest()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"latest": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"latest": rel})
}

// GET /api/v1/releases/{version} 下载指定版本的完整快照。
func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	var version int
	n, err := fmtSscanf(r.URL.Path, &version)
	if err != nil || n != 1 {
		writeErr(w, http.StatusBadRequest, "版本号无效")
		return
	}
	rel, ok := s.store.GetRelease(version)
	if !ok {
		writeErr(w, http.StatusNotFound, "版本不存在")
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// POST /api/v1/register 注册/心跳对账：上报实际版本，拿回最新全量。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req protocol.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return
	}
	if req.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "client_id 必填")
		return
	}
	latest, has, err := s.store.Touch(req.ClientID, req.ActiveVersion)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.markOnline(req.ClientID)
	resp := protocol.ReconcileResponse{TargetVersion: 0}
	if has {
		rel := latest
		resp.Latest = &rel
		resp.TargetRelease = &rel
		resp.TargetVersion = latest.Version
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/v1/ack 客户端确认（downloaded/applied/failed）。
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req protocol.AckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效: "+err.Error())
		return
	}
	if req.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "client_id 必填")
		return
	}
	if err := s.store.Ack(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.markOnline(req.ClientID)
	log.Printf("[server] 确认 client=%s v%d kind=%s", req.ClientID, req.Version, req.Kind)
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot(s.isOnline))
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

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
