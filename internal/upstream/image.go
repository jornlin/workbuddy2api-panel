// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/yreidev/workbuddy2api-panel

// image.go 生图/图生图上游接口。
//
// 官方桌面端内置 CLI 的生图链路（@tencent-ai/codebuddy-code 2.185.0，
// dist/codebuddy.js 的 ImageServiceImpl）：
//
//	POST {product.endpoint}/v2/images/generations  文生图
//	POST {product.endpoint}/v2/images/edits        图生图
//
// 请求体按模型族分发：hunyuan-* 只发 model/prompt/size/n/footnote/revise；
// gemini-* 只发 model/prompt/size/image；其余（OpenAI 系）额外发
// response_format/n/quality/style/background。响应统一是
// {"code":0,"msg":"OK","requestId":...,"data":{"created":...,"data":[{"url"|"b64_json"}],"usage":{"credit":...}}}。
//
// product.endpoint 取 /v3/config 的 data.endpoint：国内版 copilot.tencent.com，
// 国际版 www.codebuddy.ai（与对话入口 www.workbuddy.cn 不同）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yreidev/workbuddy2api-panel/internal/auth"
)

const (
	defaultImageBaseCN     = "https://copilot.tencent.com"
	defaultImageBaseGlobal = "https://www.codebuddy.ai"

	imageGenerationsPath = "/v2/images/generations"
	imageEditsPath       = "/v2/images/edits"
)

// imageTags 生成类模型标签（与 nonChatModel 同一口径）。
var imageTags = []string{"text-to-image", "image-to-image"}

// IsImageModel 报告模型标签里是否含生图能力。
func IsImageModel(tags []string) bool {
	for _, t := range tags {
		for _, want := range imageTags {
			if t == want {
				return true
			}
		}
	}
	return false
}

// ImageRequest 一次生图请求（字段与官方 CLI 对齐，未用到的字段零值）。
type ImageRequest struct {
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
	Image          []string // 图生图输入：data URL 或公网 URL
	InputFidelity  string
}

// ImageItem 单张结果。上游通常给 url；OpenAI 系模型可能给 b64_json。
type ImageItem struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}

// ImageResponse 统一后的生图结果。
type ImageResponse struct {
	Created   int64
	Data      []ImageItem
	Credit    float64
	HasCredit bool
	RequestID string
}

// ImageAPIError 上游业务错误（code != 0）。Code 供调用方区分「换号能救」还是
// 「请求本身不合法」。
type ImageAPIError struct {
	Code      int
	Msg       string
	Status    int
	RequestID string
}

func (e *ImageAPIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("image api: %s (code=%d, request_id=%s)", e.Msg, e.Code, e.RequestID)
	}
	return fmt.Sprintf("image api: %s (code=%d)", e.Msg, e.Code)
}

// imageBase 生效的生图 base：国内版 copilot.tencent.com，国际版 www.codebuddy.ai。
// config upstream.image_base_cn / image_base_global 可覆盖（自建中转等场景）。
func (c *Client) imageBase(a *auth.Auth) string {
	if c.globalOn(a) {
		if c.ImageBaseGlobal != "" {
			return c.ImageBaseGlobal
		}
		return defaultImageBaseGlobal
	}
	if c.ImageBaseCN != "" {
		return c.ImageBaseCN
	}
	return defaultImageBaseCN
}

// GenerateImage 调用生图/图生图接口。edit=true 走 /v2/images/edits。
// 超时放宽到 5 分钟（实测单张 9~11 秒，慢模型会更久）。
func (c *Client) GenerateImage(ctx context.Context, a *auth.Auth, req ImageRequest, edit bool) (*ImageResponse, error) {
	if a == nil {
		return nil, fmt.Errorf("image: nil auth")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("image: model is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("image: prompt is required")
	}
	body := buildImageBody(req, edit)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("image: marshal body: %w", err)
	}
	path := imageGenerationsPath
	if edit {
		path = imageEditsPath
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	httpReq, err := newAccountRequest(reqCtx, a, http.MethodPost, c.imageBase(a)+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Close = true
	c.frameworkHeaders(httpReq, a, c.userAgent(a))
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := doDecoded(hc, httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("image: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, imageErrorFromResponse(resp.StatusCode, raw)
	}
	return parseImageResponse(raw)
}

// buildImageBody 按官方 CLI 的 buildBaseRequestBody 分发字段：
// hunyuan-* 不发 OpenAI-only 字段（上游会对不支持的字段报 14401）。
func buildImageBody(req ImageRequest, edit bool) map[string]any {
	model := strings.TrimSpace(req.Model)
	size := strings.TrimSpace(req.Size)
	if size == "" {
		size = "1024x1024"
	}
	body := map[string]any{
		"model":  model,
		"prompt": req.Prompt,
		"size":   size,
	}
	hunyuan := strings.HasPrefix(model, "hunyuan-")
	gemini := strings.HasPrefix(model, "gemini-")
	if !hunyuan && !gemini {
		body["response_format"] = "b64_json"
		if req.ResponseFormat != "" {
			body["response_format"] = req.ResponseFormat
		}
		if req.N > 0 {
			body["n"] = req.N
		}
		if req.Quality != "" {
			body["quality"] = req.Quality
		}
		if req.Style != "" {
			body["style"] = req.Style
		}
		if req.Background != "" {
			body["background"] = req.Background
		}
	}
	if hunyuan {
		if req.N > 0 {
			body["n"] = req.N
		} else {
			body["n"] = 1
		}
		if req.Footnote != "" {
			body["footnote"] = req.Footnote
		}
		if req.Revise != nil {
			body["revise"] = map[string]any{"value": *req.Revise}
		}
	}
	if edit {
		imgs := make([]string, 0, len(req.Image))
		for _, s := range req.Image {
			if strings.TrimSpace(s) != "" {
				imgs = append(imgs, s)
			}
		}
		if len(imgs) > 0 {
			body["image"] = imgs
		}
		if req.InputFidelity != "" {
			body["input_fidelity"] = req.InputFidelity
		}
	}
	return body
}

// imageEnvelope 上游响应外壳（CN / global 同构）。
type imageEnvelope struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	RequestID string `json:"requestId"`
	Data      struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Usage *struct {
			Credit float64 `json:"credit"`
		} `json:"usage"`
	} `json:"data"`
}

// imageErrorFromResponse 非 200 响应的错误构造：上游可能用非 2xx 的 HTTP 状态承载业务错误
// （实测未知模型回 HTTP 400 + body {"code":14401,...}）。优先取 body 里的业务码，
// 解析不出来才退回 HTTP 状态 —— 否则 14401 这类参数错误会被误判成 502，
// 还会被 imageRetryable 当成可换号重试，白白多打几轮上游。
func imageErrorFromResponse(status int, raw []byte) *ImageAPIError {
	var env imageEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		return &ImageAPIError{Code: env.Code, Msg: env.Msg, RequestID: env.RequestID, Status: status}
	}
	return &ImageAPIError{Code: status, Msg: truncate(string(raw), 300), Status: status}
}

func parseImageResponse(raw []byte) (*ImageResponse, error) {
	var env imageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("image: parse response: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if env.Code != 0 {
		return nil, &ImageAPIError{Code: env.Code, Msg: env.Msg, RequestID: env.RequestID}
	}
	out := &ImageResponse{Created: env.Data.Created, RequestID: env.RequestID}
	for _, it := range env.Data.Data {
		if it.URL == "" && it.B64JSON == "" {
			continue
		}
		out.Data = append(out.Data, ImageItem{URL: it.URL, B64JSON: it.B64JSON})
	}
	if env.Data.Usage != nil {
		// usage 缺省与 credit=0 语义不同：用指针区分「没给用量」与「免费」。
		out.Credit = env.Data.Usage.Credit
		out.HasCredit = true
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("image: no image data returned (request_id: %s)", env.RequestID)
	}
	return out, nil
}

// FetchImageModels 拉该账号域的生图模型（tags 含 text-to-image / image-to-image）。
// 复用 /v3/config 目录探测，不额外发请求。
func (c *Client) FetchImageModels(a *auth.Auth) ([]ModelInfo, error) {
	byID, err := c.fetchV3ConfigModelMap(a)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, 4)
	for _, mi := range byID {
		if IsImageModel(mi.Tags) {
			out = append(out, mi)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned no image models")
	}
	return out, nil
}
