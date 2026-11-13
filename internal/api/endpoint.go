package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xhj015/webhook-gateway/internal/idgen"
	"github.com/xhj015/webhook-gateway/internal/store"
)

// maxCreateEndpointBody 创建端点请求体的上限。这个接口只需要两个短字符串。
const maxCreateEndpointBody = 64 << 10 // 64 KB

// createEndpointRequest 是 POST /api/endpoints 的请求体。
type createEndpointRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// endpointResponse 是端点的对外表示。
// 字段名用下划线风格，和数据库列名保持一致，少一层心智映射。
type endpointResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	HookPath  string `json:"hook_path"` // 调用方要 POST 的路径，省得自己拼
	CreatedAt int64  `json:"created_at"`
}

// createEndpoint 处理 POST /api/endpoints：创建一个投递目标。
//
// 没有这个接口，接收链路就没法演示——`POST /hooks/{id}` 里的 {id} 从哪来？
func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var req createEndpointRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCreateEndpointBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name 不能为空")
		return
	}

	// 为什么现在就要校验 URL：投递目标地址一旦写错，事件会一直失败到重试耗尽。
	// 在入口处拦住明显不合法的写法，比事后翻日志便宜得多。
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "url 必须是完整的 http:// 或 https:// 地址")
		return
	}

	now := time.Now().Unix()
	ep := store.Endpoint{
		ID:        idgen.Endpoint(),
		Name:      req.Name,
		URL:       req.URL,
		Enabled:   true,
		CreatedAt: now,
	}

	if err := h.store.CreateEndpoint(r.Context(), ep); err != nil {
		h.log.Error("创建端点失败", "err", err)
		writeError(w, http.StatusInternalServerError, "写入数据库失败")
		return
	}

	h.log.Info("端点已创建", "endpoint_id", ep.ID, "name", ep.Name, "url", ep.URL)

	writeJSON(w, http.StatusCreated, toEndpointResponse(ep))
}

// toEndpointResponse 把 store.Endpoint 转成对外表示。创建、列表、停用三处共用，保证字段一致。
func toEndpointResponse(ep store.Endpoint) endpointResponse {
	return endpointResponse{
		ID:        ep.ID,
		Name:      ep.Name,
		URL:       ep.URL,
		Enabled:   ep.Enabled,
		HookPath:  "/hooks/" + ep.ID,
		CreatedAt: ep.CreatedAt,
	}
}

// listEndpoints 处理 GET /api/endpoints：列出全部投递目标。
func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := h.store.ListEndpoints(r.Context())
	if err != nil {
		h.log.Error("查询端点列表失败", "err", err)
		writeError(w, http.StatusInternalServerError, "查询端点列表失败")
		return
	}

	out := make([]endpointResponse, 0, len(eps))
	for _, ep := range eps {
		out = append(out, toEndpointResponse(ep))
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

// disableEndpoint 处理 POST /api/endpoints/{id}/disable：停用一个投递目标。
//
// 幂等：对已停用的端点再停一次，仍然返回 200，不会报错。
// 停用只影响新事件，在途事件继续投完（B10）——这个语义由 store.DisableEndpoint 只改 endpoints 一行来保证。
func (h *Handler) disableEndpoint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	ep, err := h.store.GetEndpoint(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "端点不存在: "+id)
		return
	}
	if err != nil {
		h.log.Error("查询端点失败", "err", err, "endpoint_id", id)
		writeError(w, http.StatusInternalServerError, "查询端点失败")
		return
	}

	changed, err := h.store.DisableEndpoint(ctx, id, time.Now().Unix())
	if err != nil {
		h.log.Error("停用端点失败", "err", err, "endpoint_id", id)
		writeError(w, http.StatusInternalServerError, "停用端点失败")
		return
	}
	if changed {
		h.log.Info("端点已停用", "endpoint_id", id, "name", ep.Name)
	} else {
		h.log.Info("端点已处于停用状态，跳过", "endpoint_id", id)
	}

	// 停用后 enabled 必为 false，其余字段沿用刚读到的 ep
	writeJSON(w, http.StatusOK, toEndpointResponse(store.Endpoint{
		ID:        ep.ID,
		Name:      ep.Name,
		URL:       ep.URL,
		Enabled:   false,
		CreatedAt: ep.CreatedAt,
	}))
}
