// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/yreidev/workbuddy2api-panel

// images.go OpenAI 兼容的 /v1/images/generations 与 /v1/images/edits。
//
// 上游走官方桌面端内置 CLI 的同一条链路（/v2/images/generations、/v2/images/edits），
// 选号、token 刷新、积分台账与对话共用同一个账号池。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yreidev/workbuddy2api-panel/internal/auth"
	"github.com/yreidev/workbuddy2api-panel/internal/logfmt"
	"github.com/yreidev/workbuddy2api-panel/internal/reqlog"
	"github.com/yreidev/workbuddy2api-panel/internal/upstream"
)

const (
	imageModelsTTL        = 10 * time.Minute
	imageModelsFailCool   = 5 * time.Minute
	imageMaxUploadBytes   = 32 << 20 // 单张参考图上限
	imageMaxDownloadBytes = 64 << 20 // response_format=b64_json 时回读原图上限
)

// imageModelsCache 按域缓存生图模型目录（与对话目录分开：生图模型不参与 chat 选号）。
var imageModelsCache struct {
	sync.RWMutex
	byRealm map[string][]upstream.ModelInfo
	fetched map[string]time.Time
	lastErr map[string]time.Time
}

func init() {
	imageModelsCache.byRealm = map[string][]upstream.ModelInfo{}
	imageModelsCache.fetched = map[string]time.Time{}
	imageModelsCache.lastErr = map[string]time.Time{}
}

// fetchImageModels 拉某域的生图模型目录（10min 缓存 + 5min 失败负缓存）。
func (h *Handler) fetchImageModels(realm string) []upstream.ModelInfo {
	imageModelsCache.RLock()
	if infos, ok := imageModelsCache.byRealm[realm]; ok && len(infos) > 0 && time.Since(imageModelsCache.fetched[realm]) < imageModelsTTL {
		imageModelsCache.RUnlock()
		return infos
	}
	if t := imageModelsCache.lastErr[realm]; !t.IsZero() && time.Since(t) < imageModelsFailCool {
		imageModelsCache.RUnlock()
		return nil
	}
	imageModelsCache.RUnlock()

	acct := h.imageCatalogAccount(realm)
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchImageModels(acct)
	if err != nil || len(infos) == 0 {
		imageModelsCache.Lock()
		imageModelsCache.lastErr[realm] = time.Now()
		imageModelsCache.Unlock()
		return nil
	}
	imageModelsCache.Lock()
	imageModelsCache.byRealm[realm] = infos
	imageModelsCache.fetched[realm] = time.Now()
	imageModelsCache.lastErr[realm] = time.Time{}
	imageModelsCache.Unlock()
	return infos
}

// imageCatalogAccount 取拉目录用的账号：CN 优先个人号（与 /v1/models 同口径），
// global 取任一可用号。
func (h *Handler) imageCatalogAccount(realm string) *auth.Auth {
	if realm == "global" {
		return h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	}
	if uid := h.cfg.Pool.CatalogUIDForRealm("cn"); uid != "" {
		if a := h.cfg.Pool.AuthByUID(uid); a != nil {
			return a
		}
	}
	return h.cfg.Pool.PickExcludingForRealm(nil, "", "cn")
}

// imageModelRealm 解析不带前缀的生图模型该走哪个域：目录里有的域优先，国内版优先。
func (h *Handler) imageModelRealm(bare string) string {
	for _, realm := range []string{"cn", "global"} {
		for _, mi := range h.fetchImageModels(realm) {
			if mi.ID == bare {
				return realm
			}
		}
	}
	return "cn"
}

// defaultImageModel 请求未带 model 时的兜底：按入口挑该域目录里能力匹配的模型。
// 上游目录是 map 解析，顺序不稳定，必须按 tag 显式挑（否则图生图可能落到纯文生图模型）。
func (h *Handler) defaultImageModel(realm string, edit bool) string {
	want := "text-to-image"
	if edit {
		want = "image-to-image"
	}
	for _, mi := range h.fetchImageModels(realm) {
		if hasImageTag(mi.Tags, want) {
			return mi.ID
		}
	}
	if realm == "global" {
		return "hy-image-3.5"
	}
	if edit {
		return "hunyuan-image-alpha-edit"
	}
	return "hunyuan-image-alpha"
}

func hasImageTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	h.handleImage(w, r, false)
}

func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	h.handleImage(w, r, true)
}

// imageRequestBody 统一的请求形态（JSON 与 multipart 都落到这里）。
type imageRequestBody struct {
	Model          string
	Prompt         string
	Size           string
	N              int
	ResponseFormat string
	Quality        string
	Style          string
	Background     string
	Footnote       string
	Revise         *bool
	InputFidelity  string
	Image          []string
}

func (h *Handler) handleImage(w http.ResponseWriter, r *http.Request, edit bool) {
	// 生图请求也进请求日志（模型状态列靠它显示「可用」），流水行与对话同格式。
	st := newChatStat(time.Now(), nil, false)
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
	}
	defer st.done()
	fail := func(status int) {
		st.status, st.outcome = status, reqlog.OutcomeHTTPError
	}
	req, err := parseImageRequest(r, edit)
	if err != nil {
		fail(http.StatusBadRequest)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	st.model = req.Model
	model := strings.TrimSpace(req.Model)
	explicit := strings.HasPrefix(model, "cn:") || strings.HasPrefix(model, "global:")
	realm, bare := resolveModel(model)
	if bare == "" {
		if explicit {
			fail(http.StatusBadRequest)
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
			return
		}
		bare = h.defaultImageModel(realm, edit)
	} else if !explicit {
		realm = h.imageModelRealm(bare)
	}
	if strings.TrimSpace(bare) == "" {
		fail(http.StatusBadRequest)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	st.model = realm + ":" + bare
	if edit && len(req.Image) == 0 {
		fail(http.StatusBadRequest)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image is required for /v1/images/edits")
		return
	}
	if edit {
		for _, mi := range h.fetchImageModels(realm) {
			if mi.ID == bare && !hasImageTag(mi.Tags, "image-to-image") {
				fail(http.StatusBadRequest)
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("model %s does not support image-to-image; use %s", bare, h.defaultImageModel(realm, true)))
				return
			}
		}
	}

	// 客户端密钥：白名单与次数上限和对话同口径。
	keyOwner := principalFrom(r.Context())
	admitted := time.Now()
	admitTarget := "workbuddy:cn"
	if realm == "global" {
		admitTarget = "workbuddy:global"
	}
	if status := h.admitKey(w, keyOwner, admitTarget, bare, admitted); status != 0 {
		fail(status)
		return
	}
	success := false
	if keyOwner.key != nil {
		defer func() {
			if !success {
				h.cfg.Keys.Refund(keyOwner.key.ID, admitted)
			}
		}()
	}

	upReq := upstream.ImageRequest{
		Model:          bare,
		Prompt:         req.Prompt,
		Size:           req.Size,
		N:              req.N,
		ResponseFormat: req.ResponseFormat,
		Quality:        req.Quality,
		Style:          req.Style,
		Background:     req.Background,
		Footnote:       req.Footnote,
		Revise:         req.Revise,
		Image:          req.Image,
		InputFidelity:  req.InputFidelity,
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, bare, realm)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}
		held := acct.UID
		st.uid, st.nick, st.attempts = acct.UID, acct.Nickname, st.attempts+1
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				h.cfg.Pool.Release(held)
				lastErr = err
				continue
			}
		}
		res, err := h.cfg.Upstream.GenerateImage(r.Context(), acct, upReq, edit)
		h.cfg.Pool.Release(held)
		if err == nil {
			h.cfg.Pool.NoteSuccess(held)
			st.status, st.outcome = http.StatusOK, reqlog.OutcomeSuccess
			st.credit, st.hasCredit = res.Credit, res.HasCredit
			if res.HasCredit {
				h.cfg.Pool.NoteModelCost(held, bare, res.Credit, 0)
			}
			items, ierr := imageResultItems(r.Context(), res, req.ResponseFormat == "b64_json")
			if ierr != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", ierr.Error())
				return
			}
			resp := map[string]any{
				"created": res.Created,
				"data":    items,
				"model":   req.Model,
			}
			if res.RequestID != "" {
				resp["request_id"] = res.RequestID
			}
			success = true
			writeJSON(w, http.StatusOK, resp)
			return
		}
		lastErr = err
		log.Printf("WARN: [server] image %s failed acct=%s model=%s realm=%s: %v",
			map[bool]string{true: "edit", false: "generation"}[edit], logfmt.Label(acct.UID, acct.Nickname), bare, realm, err)
		if !imageRetryable(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy account for image generation")
		fail(http.StatusServiceUnavailable)
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("no available account for image model %s (realm %s); try another model or retry later", bare, realm))
		return
	}
	status, _ := imageErrorStatus(lastErr)
	fail(status)
	writeImageError(w, lastErr)
}

// parseImageRequest JSON / multipart 两种入口归一。
func parseImageRequest(r *http.Request, edit bool) (*imageRequestBody, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(imageMaxUploadBytes); err != nil {
			return nil, fmt.Errorf("parse multipart: %w", err)
		}
		out := &imageRequestBody{
			Model:          r.FormValue("model"),
			Prompt:         r.FormValue("prompt"),
			Size:           r.FormValue("size"),
			ResponseFormat: r.FormValue("response_format"),
			Quality:        r.FormValue("quality"),
			Style:          r.FormValue("style"),
			Background:     r.FormValue("background"),
			Footnote:       r.FormValue("footnote"),
			InputFidelity:  r.FormValue("input_fidelity"),
		}
		if n := r.FormValue("n"); n != "" {
			fmt.Sscanf(n, "%d", &out.N)
		}
		if rv := r.FormValue("revise"); rv != "" {
			v := rv == "true" || rv == "1"
			out.Revise = &v
		}
		for _, field := range []string{"image", "image[]"} {
			if r.MultipartForm == nil {
				continue
			}
			for _, fh := range r.MultipartForm.File[field] {
				du, err := imageFileToDataURL(fh)
				if err != nil {
					return nil, err
				}
				out.Image = append(out.Image, du)
			}
		}
		if out.Prompt == "" {
			return nil, errors.New("prompt is required")
		}
		return out, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, imageMaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > imageMaxUploadBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes", imageMaxUploadBytes)
	}
	var raw struct {
		Model          string          `json:"model"`
		Prompt         string          `json:"prompt"`
		Size           string          `json:"size"`
		N              int             `json:"n"`
		ResponseFormat string          `json:"response_format"`
		Quality        string          `json:"quality"`
		Style          string          `json:"style"`
		Background     string          `json:"background"`
		Footnote       string          `json:"footnote"`
		Revise         *bool           `json:"revise"`
		InputFidelity  string          `json:"input_fidelity"`
		Image          json.RawMessage `json:"image"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	out := &imageRequestBody{
		Model: raw.Model, Prompt: raw.Prompt, Size: raw.Size, N: raw.N,
		ResponseFormat: raw.ResponseFormat, Quality: raw.Quality, Style: raw.Style,
		Background: raw.Background, Footnote: raw.Footnote, Revise: raw.Revise,
		InputFidelity: raw.InputFidelity,
	}
	if len(raw.Image) > 0 && string(raw.Image) != "null" {
		var one string
		if err := json.Unmarshal(raw.Image, &one); err == nil {
			out.Image = []string{one}
		} else {
			var many []string
			if err := json.Unmarshal(raw.Image, &many); err != nil {
				return nil, errors.New("image must be a string or an array of strings")
			}
			out.Image = many
		}
	}
	if out.Prompt == "" {
		return nil, errors.New("prompt is required")
	}
	return out, nil
}

func imageFileToDataURL(fh *multipart.FileHeader) (string, error) {
	if fh.Size > imageMaxUploadBytes {
		return "", fmt.Errorf("image %s exceeds %d bytes", fh.Filename, imageMaxUploadBytes)
	}
	f, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("open %s: %w", fh.Filename, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, imageMaxUploadBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fh.Filename, err)
	}
	if len(b) > imageMaxUploadBytes {
		return "", fmt.Errorf("image %s exceeds %d bytes", fh.Filename, imageMaxUploadBytes)
	}
	ct := fh.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = http.DetectContentType(b)
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

// imageResultItems 转成 OpenAI images 响应条目。请求 b64_json 时把上游 URL 回读成 base64
// （上游 hunyuan 系不认 response_format，恒返回 URL）。
func imageResultItems(ctx context.Context, res *upstream.ImageResponse, wantB64 bool) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(res.Data))
	for _, it := range res.Data {
		m := map[string]any{}
		switch {
		case it.B64JSON != "":
			m["b64_json"] = it.B64JSON
		case it.URL != "" && wantB64:
			b64, err := downloadImageB64(ctx, it.URL)
			if err != nil {
				m["url"] = it.URL // 回读失败退 URL，不让整个请求失败
			} else {
				m["b64_json"] = b64
			}
		case it.URL != "":
			m["url"] = it.URL
		}
		if len(m) > 0 {
			items = append(items, m)
		}
	}
	if len(items) == 0 {
		return nil, errors.New("upstream returned no usable image data")
	}
	return items, nil
}

func downloadImageB64(ctx context.Context, url string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download image: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, imageMaxDownloadBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > imageMaxDownloadBytes {
		return "", errors.New("download image: too large")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// imageRetryable 上游业务码是否值得换号重试：
//   - 11102/11103 service info not found / backend not supported：换号或换域可能恢复
//   - 11158 账号类型不支持该模型：换号（付费号可能可用）
//   - 14401/11101 参数/请求体不合法：换号也没用
func imageRetryable(err error) bool {
	var ie *upstream.ImageAPIError
	if errors.As(err, &ie) {
		switch ie.Code {
		case 11102, 11103, 11158, 11133, 11135, 6004, 10001:
			return true
		case 14401, 11101:
			return false
		}
		if ie.Status == http.StatusUnauthorized || ie.Status == http.StatusForbidden {
			return true
		}
		return ie.Status >= 500
	}
	// 传输层错误换号重试。
	return true
}

// imageErrorStatus 上游错误 → 回给客户端的 HTTP 状态与错误码。
func imageErrorStatus(err error) (int, string) {
	var ie *upstream.ImageAPIError
	if errors.As(err, &ie) {
		switch ie.Code {
		case 14401, 11101:
			return http.StatusBadRequest, "invalid_request"
		case 11102, 11103, 11158, 11133, 11135, 6004, 10001:
			return http.StatusServiceUnavailable, "model_unavailable"
		}
		return http.StatusBadGateway, "upstream_error"
	}
	return http.StatusBadGateway, "upstream_error"
}

func writeImageError(w http.ResponseWriter, err error) {
	status, code := imageErrorStatus(err)
	writeOpenAIError(w, status, code, err.Error())
}

// imageModelEntry /v1/models 里的生图模型条目：与对话条目同构，另标 type=image 与
// supports_image_generation，客户端据此知道该走 /v1/images/*。
func imageModelEntry(realm string, mi upstream.ModelInfo) map[string]any {
	entry := map[string]any{
		"id":                        realm + ":" + mi.ID,
		"object":                    "model",
		"created":                   1753600000,
		"owned_by":                  "workbuddy",
		"type":                      "image",
		"supports_image_generation": true,
	}
	return applyModelInfoFields(entry, mi)
}
