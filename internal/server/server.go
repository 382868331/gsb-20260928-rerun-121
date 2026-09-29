package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"configdist/internal/protocol"
	"configdist/internal/store"
)

type serverData struct {
	Versions map[string]protocol.Config      `json:"versions"`
	Desired  protocol.Config                 `json:"desired"`
	Clients  map[string]protocol.ClientState `json:"clients"`
}

type Server struct {
	mu       sync.Mutex
	st       *store.Store[serverData]
	http     *http.Server
	clientRT http.RoundTripper // 回连客户端使用的传输，默认真实网络
}

func New(dataPath string) (*Server, error) {
	st, err := store.New(dataPath, serverData{
		Versions: map[string]protocol.Config{},
		Clients:  map[string]protocol.ClientState{},
	})
	if err != nil {
		return nil, err
	}
	s := &Server{st: st}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /publish", s.handlePublish)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("GET /manifest", s.handleManifest)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("POST /report", s.handleReport)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)
	mux.HandleFunc("GET /status", s.handleStatus)
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s, nil
}

// Handler 返回路由处理器，便于无端口内存测试。
func (s *Server) Handler() http.Handler { return s.http.Handler }

// SetClientTransport 注入服务端回连客户端时使用的传输层。
func (s *Server) SetClientTransport(rt http.RoundTripper) { s.clientRT = rt }

func (s *Server) ListenAndServe(addr string) error {
	s.http.Addr = addr
	return s.http.ListenAndServe()
}

func (s *Server) Close() error { return s.http.Close() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// publish：发布新的完整版本。版本序号必须递增，避免旧发布覆盖新版本。
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var cfg protocol.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if cfg.ID == "" || cfg.Sequence <= 0 {
		writeJSON(w, 400, map[string]string{"error": "version id 与正整数 sequence 必填"})
		return
	}
	err := s.st.Update(func(d *serverData) {
		if d.Desired.ID != "" && cfg.Sequence <= d.Desired.Sequence {
			return
		}
		d.Versions[cfg.ID] = cfg
		d.Desired = cfg
		// 新期望产生后，所有已注册客户端都有待处理项。
		for id, c := range d.Clients {
			if c.AppliedID != cfg.ID {
				c.PendingID = cfg.ID
				d.Clients[id] = c
			}
		}
	})
	if err == nil {
		var stale bool
		s.st.View(func(d *serverData) { stale = d.Desired.ID != cfg.ID })
		if stale {
			err = fmt.Errorf("版本 %s(seq=%d) 不新于当前期望版本", cfg.ID, cfg.Sequence)
		}
	}
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"desiredId": cfg.ID})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, 400, map[string]string{"error": "客户端 id 必填"})
		return
	}
	err := s.st.Update(func(d *serverData) {
		c, ok := d.Clients[req.ID]
		if !ok {
			c = protocol.ClientState{ID: req.ID}
		}
		c.Addr = req.Addr
		if d.Desired.ID != "" && c.AppliedID != d.Desired.ID {
			c.PendingID = d.Desired.ID
		}
		d.Clients[req.ID] = c
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	s.st.View(func(d *serverData) {
		writeJSON(w, 200, protocol.Manifest{DesiredID: d.Desired.ID, DesiredSeq: d.Desired.Sequence})
	})
}

// 客户端可以缺中间版本，直接按 ID 拉取最新完整快照。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		s.st.View(func(d *serverData) { id = d.Desired.ID })
	}
	var cfg protocol.Config
	var found bool
	s.st.View(func(d *serverData) { cfg, found = d.Versions[id] })
	if !found {
		writeJSON(w, 404, map[string]string{"error": "版本不存在: " + id})
		return
	}
	writeJSON(w, 200, cfg)
}

// report：关键语义——
//  1. downloaded 仅记录下载，绝不等于 applied；
//  2. applied 必须携带与服务端记录一致的版本；
//  3. 重复/更旧的确认不能让 applied 回退；
//  4. failed 保留失败原因与待处理版本。
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req protocol.ReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	err := s.st.Update(func(d *serverData) {
		c, ok := d.Clients[req.ClientID]
		if !ok {
			return
		}
		switch req.Stage {
		case protocol.StageDownloaded:
			// 下载确认不是成功，不清除 pending、不改 applied。
			c.DownloadedID = req.VersionID
		case protocol.StageApplied:
			// 幂等且单调：已应用更新或相同版本才接受，旧版本确认被忽略。
			if seqOf(d, c.AppliedID) < seqOf(d, req.VersionID) {
				c.AppliedID = req.VersionID
				c.FailureReason = ""
				if c.PendingID == req.VersionID {
					c.PendingID = ""
				}
			}
			c.DownloadedID = req.VersionID
		case protocol.StageFailed:
			// 应用失败：保留失败原因与待处理状态，applied 不变（客户端保留原版）。
			c.FailureReason = req.Reason
			if d.Desired.ID != "" && c.AppliedID != d.Desired.ID {
				c.PendingID = d.Desired.ID
			}
		default:
			return
		}
		d.Clients[req.ClientID] = c
	})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var known bool
	s.st.View(func(d *serverData) { _, known = d.Clients[req.ClientID] })
	if !known {
		writeJSON(w, 400, map[string]string{"error": "未注册客户端: " + req.ClientID})
		return
	}
	w.WriteHeader(204)
}

func seqOf(d *serverData, id string) int {
	if v, ok := d.Versions[id]; ok {
		return v.Sequence
	}
	return 0
}

// reconcile：服务端主动回调各客户端 /actual，检查真实在用版本，
// 重启或短暂断开后用实际状态纠正记录，而不是只信下载确认。
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	type result struct {
		ClientID string `json:"clientId"`
		Active   string `json:"activeVersion"`
		Online   bool   `json:"online"`
		Error    string `json:"error,omitempty"`
	}
	var targets map[string]protocol.ClientState
	s.st.View(func(d *serverData) {
		targets = make(map[string]protocol.ClientState, len(d.Clients))
		for k, v := range d.Clients {
			targets[k] = v
		}
	})
	results := make([]result, 0, len(targets))
	for id, c := range targets {
		res := result{ClientID: id}
		httpc := &http.Client{Timeout: 2 * time.Second}
		if s.clientRT != nil {
			httpc.Transport = s.clientRT
		}
		resp, err := httpc.Get("http://" + c.Addr + "/actual?from=server")
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		if resp.StatusCode != 200 {
			res.Error = fmt.Sprintf("status %d", resp.StatusCode)
			resp.Body.Close()
			results = append(results, res)
			continue
		}
		var ar protocol.ActualResponse
		if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
			resp.Body.Close()
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		resp.Body.Close()
		res.Online = true
		res.Active = ar.ActiveVersion
		_ = s.st.Update(func(d *serverData) {
			cc := d.Clients[id]
			cc.ObservedID = ar.ActiveVersion
			// 用客户端实际状态纠正：实际已是新版本才算成功；实际仍是旧版本则保持待处理。
			if ar.ActiveVersion != "" && seqOf(d, ar.ActiveVersion) > seqOf(d, cc.AppliedID) {
				cc.AppliedID = ar.ActiveVersion
				cc.FailureReason = ""
				if cc.PendingID == ar.ActiveVersion {
					cc.PendingID = ""
				}
			}
			if d.Desired.ID != "" && ar.ActiveVersion != d.Desired.ID && seqOf(d, ar.ActiveVersion) < d.Desired.Sequence {
				cc.PendingID = d.Desired.ID
			}
			d.Clients[id] = cc
		})
		results = append(results, res)
	}
	writeJSON(w, 200, results)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.st.View(func(d *serverData) {
		clients := make(map[string]protocol.ClientState, len(d.Clients))
		for k, v := range d.Clients {
			clients[k] = v
		}
		writeJSON(w, 200, protocol.StatusResponse{DesiredID: d.Desired.ID, Clients: clients})
	})
}
