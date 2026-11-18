// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/yreidev/workbuddy2api-panel

package session

import "testing"

// 绑定号只是在途满（ServableForModel 仍为 true）时，ResolveForModel 保留绑定并返回它，
// 不再重新分配——由选号侧等待/降级；重新分配等于换号，按号缓存的前缀整段作废。
func TestResolveForModelKeepsBusyBinding(t *testing.T) {
	servable := true
	r := New(Config{
		AvailableForModel: func(model, realm string, skip map[string]bool) []string {
			return []string{"u2", "u3"} // u1 忙：可用集里没有
		},
		AssignableForModel: func(model, realm string, skip map[string]bool) []string {
			return []string{"u2", "u3"}
		},
		ServableForModel: func(uid, model, realm string) bool {
			return servable && uid == "u1"
		},
	})
	r.Bind("k", "u1")

	uid, ok := r.ResolveForModel("k", "m", "", nil)
	if !ok || uid != "u1" {
		t.Fatalf("忙但可服务的绑定号应保留，got=%q ok=%v", uid, ok)
	}

	// skip（客户端密钥分组外）时不保留：必须重分配。
	uid, ok = r.ResolveForModel("k", "m", "", map[string]bool{"u1": true})
	if !ok || uid == "u1" {
		t.Fatalf("skip 里的绑定号不应返回，got=%q ok=%v", uid, ok)
	}

	// 真的不可服务 → 重分配。
	r.Bind("k", "u1")
	servable = false
	uid, ok = r.ResolveForModel("k", "m", "", nil)
	if !ok || uid == "u1" {
		t.Fatalf("不可服务的绑定号应重分配，got=%q ok=%v", uid, ok)
	}
}

// 未注入 ServableForModel 时保持旧语义：忙即视为不可用，走重新分配。
func TestResolveForModelBusyWithoutCallbackReassigns(t *testing.T) {
	r := New(Config{
		AvailableForModel: func(model, realm string, skip map[string]bool) []string {
			return []string{"u2", "u3"}
		},
		AssignableForModel: func(model, realm string, skip map[string]bool) []string {
			return []string{"u2", "u3"}
		},
	})
	r.Bind("k", "u1")
	uid, ok := r.ResolveForModel("k", "m", "", nil)
	if !ok || uid == "u1" {
		t.Fatalf("未注入软校验时应重分配，got=%q ok=%v", uid, ok)
	}
}
