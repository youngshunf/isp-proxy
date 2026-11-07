package agentproto

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Server 是控制面实现的节点接口。nodeID 已由处理器从客户端证书取出并校验格式；
// 实现负责确认节点存在且可用，否则返回 ErrUnknownNode。
type Server interface {
	// Desired 返回节点当前的完整期望状态。应当是一次快速读取，不阻塞等待变化（长轮询由处理器完成）。
	Desired(ctx context.Context, nodeID string) (*Desired, error)
	// Report 接收上报：以 GREATEST 合并 used_total，记录已应用版本与节点状态。必须幂等。
	Report(ctx context.Context, nodeID string, r *Report) (*ReportAck, error)
	// ProbeResults 记录探测结果。按 ProbeID 幂等：已完成的探测再次回传时直接忽略。
	ProbeResults(ctx context.Context, nodeID string, results []ProbeResult) error
}

// ErrUnknownNode 表示证书对应的节点不存在或已停用（处理器返回 403）。
var ErrUnknownNode = errors.New("agentproto: 未知或已停用的节点")

// Hub 唤醒正在长轮询的节点。控制面在提交影响某节点期望状态的事务后调用 Notify
// （例如收到 PostgreSQL NOTIFY 时）。零值不可用，用 NewHub 创建。
type Hub struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

// NewHub 创建 Hub。
func NewHub() *Hub { return &Hub{ch: map[string]chan struct{}{}} }

// Changed 返回一个在 nodeID 下一次 Notify 时关闭的通道。应在读取期望状态之前取得，避免漏掉通知。
func (h *Hub) Changed(nodeID string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.ch[nodeID]
	if !ok {
		c = make(chan struct{})
		h.ch[nodeID] = c
	}
	return c
}

// Notify 唤醒 nodeID 的全部等待者。
func (h *Hub) Notify(nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.ch[nodeID]; ok {
		close(c)
		delete(h.ch, nodeID)
	}
}

// NotifyAll 唤醒所有节点的等待者。
func (h *Hub) NotifyAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, c := range h.ch {
		close(c)
		delete(h.ch, id)
	}
}

// HandlerOptions 配置 NewHandler。
type HandlerOptions struct {
	// MaxWait 是长轮询最长等待，默认 MaxLongPoll（25 秒）。
	MaxWait time.Duration
	// Logger 默认 slog.Default()。
	Logger *slog.Logger
	// NodeID 从请求取节点身份，默认 NodeIDFromRequest（客户端证书 CN）。仅测试替换。
	NodeID func(*http.Request) (string, error)
}

// maxBody 限制请求体大小。上报按每订阅约 200 字节计，8 MiB 足够数万订阅。
const maxBody = 8 << 20

// NewHandler 返回挂载 /agent/v1/* 的处理器。必须放在要求客户端证书的 TLS 监听器后面（ServerTLSConfig）。
func NewHandler(srv Server, hub *Hub, opts HandlerOptions) http.Handler {
	if opts.MaxWait <= 0 {
		opts.MaxWait = MaxLongPoll
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NodeID == nil {
		opts.NodeID = NodeIDFromRequest
	}
	if hub == nil {
		hub = NewHub()
	}
	h := &handler{srv: srv, hub: hub, opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathDesired, h.desired)
	mux.HandleFunc("POST "+PathReport, h.report)
	mux.HandleFunc("POST "+PathProbeResults, h.probeResults)
	return mux
}

type handler struct {
	srv  Server
	hub  *Hub
	opts HandlerOptions
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, &Error{Code: code, Message: msg})
}

func (h *handler) node(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := h.opts.NodeID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return "", false
	}
	return id, true
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, node string, err error) {
	if errors.Is(err, ErrUnknownNode) {
		writeErr(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if r.Context().Err() != nil {
		return // 客户端已断开
	}
	h.opts.Logger.Error("agentproto: 处理节点请求失败", "node", node, "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", "控制面内部错误")
}

func (h *handler) desired(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("after")
	var after uint64
	waitForNewer := q != ""
	if waitForNewer {
		var err error
		if after, err = strconv.ParseUint(q, 10, 64); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "after 必须是非负整数")
			return
		}
	}
	timer := time.NewTimer(h.opts.MaxWait)
	defer timer.Stop()
	for {
		changed := h.hub.Changed(node)
		d, err := h.srv.Desired(r.Context(), node)
		if err != nil {
			h.fail(w, r, node, err)
			return
		}
		// 修订号与 agent 已知的不同就返回：正常情况下是更新；若控制面从备份恢复导致修订号回退，
		// 也立即返回整份状态让 agent 重新对账（agent 仍只接受版本更高的订阅条目）。
		if !waitForNewer || d.Revision != after {
			w.Header().Set("X-Revision", strconv.FormatUint(d.Revision, 10))
			writeJSON(w, http.StatusOK, d)
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			w.Header().Set("X-Revision", strconv.FormatUint(d.Revision, 10))
			w.WriteHeader(http.StatusNotModified)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "请求体不是有效的 JSON: "+err.Error())
		return false
	}
	return true
}

func (h *handler) report(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	var rep Report
	if !decode(w, r, &rep) {
		return
	}
	if err := rep.Validate(); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid", err.Error())
		return
	}
	ack, err := h.srv.Report(r.Context(), node, &rep)
	if err != nil {
		h.fail(w, r, node, err)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

func (h *handler) probeResults(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	var body ProbeResults
	if !decode(w, r, &body) {
		return
	}
	for i, res := range body.Results {
		if res.ProbeID == "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid", "results["+strconv.Itoa(i)+"] 缺少 probe_id")
			return
		}
	}
	if err := h.srv.ProbeResults(r.Context(), node, body.Results); err != nil {
		h.fail(w, r, node, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
