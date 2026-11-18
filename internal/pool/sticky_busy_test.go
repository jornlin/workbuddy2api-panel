// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/yreidev/workbuddy2api-panel

package pool

import (
	"testing"

	"github.com/yreidev/workbuddy2api-panel/internal/auth"
)

// 粘性软校验：在途满只是「忙」，不算不可服务——粘性解绑只在真的用不了时发生。
// 换号会让按号缓存的上游（deepseek）整段前缀作废，所以「忙」不能成为解绑理由。
func TestServableByUIDForModelTreatsBusyAsServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)

	if !p.ServableByUIDForModel("u1", "", "") {
		t.Fatal("空闲号应可服务")
	}
	if !p.Acquire("u1") {
		t.Fatal("占名额失败")
	}
	if !p.ServableByUIDForModel("u1", "", "") {
		t.Fatal("在途满只是忙，软校验应为 true")
	}
	if p.PickByUIDForModel("u1", "") != nil {
		t.Fatal("严格校验应挡住在途满的号")
	}
	p.Release("u1")
	if p.PickByUIDForModel("u1", "") == nil {
		t.Fatal("释放后严格校验应放行")
	}

	p.Disable("u1", "test")
	if p.ServableByUIDForModel("u1", "", "") {
		t.Fatal("禁用号的软校验应为 false")
	}
}

// 不在池里的 uid 也不可服务。
func TestServableByUIDForModelUnknownUID(t *testing.T) {
	p := New("")
	if p.ServableByUIDForModel("nope", "", "") {
		t.Fatal("未知 uid 应为 false")
	}
}
