// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/yreidev/workbuddy2api-panel

package pool

import (
	"testing"
	"time"

	"github.com/yreidev/workbuddy2api-panel/internal/auth"
)

// 选域判定必须与真正选号同口径：能判到号就必须选得到，否则国内版优先会在
// 「判有号、选不到」之间反复重选，轮转耗尽后回 503。
func TestPickableForRealmMatchesPick(t *testing.T) {
	cn := &auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	gl := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := New("")
	p.SetMaxInFlight(1)
	p.Add(cn)
	p.Add(gl)

	for _, tc := range []struct{ realm string }{{"cn"}, {"global"}} {
		if !p.PickableForRealm(nil, "m", tc.realm) {
			t.Fatalf("%s 空闲号应可判定为可选", tc.realm)
		}
		if p.PickExcludingForRealm(nil, "m", tc.realm) == nil {
			t.Fatalf("%s 判定可选时必须真的选得到", tc.realm)
		}
	}

	// 在途满：两者都必须判不可选。
	if !p.Acquire("cn1") {
		t.Fatal("Acquire cn1 失败")
	}
	if p.PickableForRealm(nil, "m", "cn") {
		t.Fatal("在途满的号不算可选")
	}
	if p.PickExcludingForRealm(nil, "m", "cn") != nil {
		t.Fatal("在途满时 pick 不应返回该号")
	}
	p.Release("cn1")

	// 另一个域不受影响。
	if !p.PickableForRealm(nil, "m", "global") {
		t.Fatal("global 域应不受 cn 在途影响")
	}
}

// 空池、tried 排除、禁用三种情况都判不可选。
func TestPickableForRealmEmptyAndExcluded(t *testing.T) {
	p := New("")
	if p.PickableForRealm(nil, "m", "cn") {
		t.Fatal("空池应为 false")
	}
	a := &auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p.Add(a)
	if !p.PickableForRealm(nil, "m", "cn") {
		t.Fatal("有号应为 true")
	}
	if p.PickableForRealm(map[string]bool{"cn1": true}, "m", "cn") {
		t.Fatal("tried 里的号不算可选")
	}
	p.Disable("cn1", "test")
	if p.PickableForRealm(nil, "m", "cn") {
		t.Fatal("禁用号不算可选")
	}
	_ = time.Now
}
