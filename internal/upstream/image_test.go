// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only

package upstream

import (
	"errors"
	"testing"
)

func TestBuildImageBodyBranches(t *testing.T) {
	revise := false
	hunyuan := buildImageBody(ImageRequest{
		Model: "hunyuan-image-alpha", Prompt: "猫", N: 2,
		ResponseFormat: "b64_json", Quality: "hd", Footnote: "note", Revise: &revise,
	}, false)
	if _, ok := hunyuan["response_format"]; ok {
		t.Error("hunyuan 不应收到 response_format（上游会 14401）")
	}
	if got := hunyuan["n"]; got != 2 {
		t.Errorf("hunyuan n=%v want 2", got)
	}
	if hunyuan["footnote"] != "note" {
		t.Error("hunyuan 应透传 footnote")
	}
	if _, ok := hunyuan["revise"]; !ok {
		t.Error("hunyuan 应透传 revise")
	}

	openai := buildImageBody(ImageRequest{
		Model: "gpt-image-3", Prompt: "猫", N: 3, Quality: "hd", Style: "vivid", Background: "opaque",
	}, false)
	if openai["response_format"] != "b64_json" {
		t.Errorf("OpenAI 系缺省 response_format=%v want b64_json", openai["response_format"])
	}
	if openai["n"] != 3 || openai["quality"] != "hd" || openai["style"] != "vivid" || openai["background"] != "opaque" {
		t.Errorf("OpenAI 系可选字段未透传: %#v", openai)
	}

	gemini := buildImageBody(ImageRequest{Model: "gemini-image-1", Prompt: "猫", N: 2}, false)
	for _, k := range []string{"response_format", "n", "quality", "style", "background"} {
		if _, ok := gemini[k]; ok {
			t.Errorf("gemini 不应收到 %s", k)
		}
	}

	edit := buildImageBody(ImageRequest{
		Model: "hunyuan-image-alpha-edit", Prompt: "改", Image: []string{"data:image/png;base64,QQ=="},
		InputFidelity: "high",
	}, true)
	if imgs, ok := edit["image"].([]string); !ok || len(imgs) != 1 {
		t.Errorf("图生图 image=%#v", edit["image"])
	}
	if edit["input_fidelity"] != "high" {
		t.Error("input_fidelity 未透传")
	}
}

func TestParseImageResponse(t *testing.T) {
	res, err := parseImageResponse([]byte(`{"code":0,"msg":"OK","requestId":"r1","data":{"created":1,"data":[{"url":"https://x/a.png"},{"b64_json":"QQ=="}],"usage":{"credit":0.5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || len(res.Data) != 2 || !res.HasCredit || res.Credit != 0.5 || res.RequestID != "r1" {
		t.Errorf("解析结果不符: %#v", res)
	}

	if _, err := parseImageResponse([]byte(`{"code":0,"data":{"data":[]}}`)); err == nil {
		t.Error("空图片列表应报错")
	}

	_, err = parseImageResponse([]byte(`{"code":14401,"msg":"bad prompt","requestId":"r2"}`))
	var ie *ImageAPIError
	if !errors.As(err, &ie) || ie.Code != 14401 {
		t.Errorf("业务错误未归类: %v", err)
	}
}

func TestImageErrorFromResponsePrefersBodyCode(t *testing.T) {
	// 实测：未知模型回 HTTP 400 + body 业务码 14401，必须保留业务码才能映射成 400/不重试。
	err := imageErrorFromResponse(400, []byte(`{"code":14401,"msg":"Image model [x] route config not found","requestId":"r1"}`))
	var ie *ImageAPIError
	if !errors.As(err, &ie) {
		t.Fatalf("want ImageAPIError, got %T", err)
	}
	if ie.Code != 14401 {
		t.Errorf("Code=%d want 14401（应取 body 业务码而非 HTTP 状态）", ie.Code)
	}
	if ie.Status != 400 {
		t.Errorf("Status=%d want 400", ie.Status)
	}
	if ie.RequestID != "r1" {
		t.Errorf("RequestID=%q want r1", ie.RequestID)
	}

	// body 不是 JSON 外壳时退回 HTTP 状态，消息保留原文。
	err = imageErrorFromResponse(503, []byte("gateway down"))
	if !errors.As(err, &ie) || ie.Code != 503 || ie.Msg != "gateway down" {
		t.Errorf("fallback got %#v", ie)
	}
}
