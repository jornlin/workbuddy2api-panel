// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/yreidev/workbuddy2api-panel

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yreidev/workbuddy2api-panel/internal/auth"
	"github.com/yreidev/workbuddy2api-panel/internal/session"
)

// 绑定号余额触底（floor 拦截）时，只让本轮换号，不能解绑：余额是慢变量，
// 签到/充值回升后会话还要回到同一个号，按号缓存的前缀才不丢。
// 与「冷却/禁用/模型限额」必须解绑的语义区分开（见 sticky_busy_test.go 的 session 版）。
func TestChatStickyFloorKeepsBinding(t *testing.T) {
	fastRotate(t)
	u1 := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	u2 := &auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999}
	p := testPoolWith(u1, u2)
	p.SetCreditFloor(100)
	p.SetCredits("u1", 50, 100)                                         // u1 触底
	p.SetModelRateOf(func(realm, model string) string { return "0.5" }) // 目录倍率 >0 → 判收费
	store := newBindStore()
	sess := session.New(session.Config{
		TTL: time.Minute, Store: store,
		Available:         p.AvailableUIDs,
		AvailableForModel: p.WeightedAvailableUIDsForModelRealm,
		ServableForModel:  p.ServableByUIDForModel,
	})
	sess.Bind("conv-1", "u1")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"conversation_id":"conv-1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if store.delCnt != 0 {
		t.Fatalf("积分触底只是慢变量，不应解绑（delCnt=%d）", store.delCnt)
	}
}
