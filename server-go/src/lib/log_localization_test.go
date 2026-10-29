package lib_test

import (
	"testing"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

func TestNormalizeLogContentForStorage(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// Exact-content table.
		{"exact password", "password", "修改密码"},
		{"exact 2fa setup", "Started 2FA setup", "开始设置双因素认证"},
		// Optional refund group present.
		{
			"batch delete with refund",
			`Batch deleted instance "vm1" on host "node-a" (refund: ¥12.50)`,
			`批量删除节点 "node-a" 上的实例 "vm1"（退款：¥12.50）`,
		},
		// Optional refund group absent.
		{
			"batch delete without refund",
			`Batch deleted instance "vm1" on host "node-a"`,
			`批量删除节点 "node-a" 上的实例 "vm1"`,
		},
		// Nested translateLogDetails on the bracket payload.
		{
			"created package details",
			`Created package "Basic" [Memory: 2GB, Disk: 20GB, url: https://x]`,
			`创建套餐 "Basic" [内存: 2GB, 磁盘: 20GB, 地址: https://x]`,
		},
		// Nested translateLogError on the trailing message. "Failed to run:" has
		// no trailing \s* in the TypeScript, so the space after it is preserved.
		{
			"failed create instance",
			`Failed to create instance "vm9": Failed to run: not authorized`,
			`创建实例 "vm9" 失败：执行失败： 未授权`,
		},
		{
			"queued start task",
			`Queued start task for instance "vm2"`,
			`已为实例 "vm2" 创建启动任务`,
		},
		// Optional reason group present / absent.
		{
			"suspended with reason",
			`Suspended instance "vm3" (reason: payment overdue)`,
			`暂停实例 "vm3"（原因：payment overdue）`,
		},
		{"suspended without reason", `Suspended instance "vm3"`, `暂停实例 "vm3"`},
		{
			"failed add port mapping",
			`Failed to add port mapping: request returned error: timeout`,
			`添加端口映射失败：请求返回错误： timeout`,
		},
		{
			"released quota",
			`Released quota for package "P1": CPU +2, Memory +4GB on 3 host(s)`,
			`为套餐 "P1" 释放配额：CPU +2，内存 +4GB，作用节点 3 个`,
		},
		{
			"system update start",
			`Started system update from 1.3.6 to 1.3.7`,
			`开始系统更新：1.3.6 -> 1.3.7`,
		},
		// Fallback: no structured match, translateLogError only.
		{"fallback unknown", "unknown", "unknown"},
	}

	for _, tc := range cases {
		if got := lib.NormalizeLogContentForStorage(tc.input); got != tc.want {
			t.Errorf("%s: NormalizeLogContentForStorage(%q) = %q, want %q", tc.name, tc.input, got, tc.want)
		}
	}
}
