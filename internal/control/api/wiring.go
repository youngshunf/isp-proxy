package api

import (
	"context"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/admin"
)

// coreParts 是 ctl-core（账本、订单、订阅、库存、任务、用量、节点同步、检测、告警、换出口申请、调度）
// 提供给平台的部分。
type coreParts struct {
	domains    []Domain
	adminHooks admin.Hooks
	agent      agentproto.Server
	hub        *agentproto.Hub
	background []func(ctx context.Context)
}

// wireCore 构造 ctl-core 的业务包。这是接入 ctl-core 的唯一位置：
//
// TODO(ctl-core)：合入 ctl-core 后在这里按 d.Pool、d.Box、d.Now 构造各业务包，
//   - 把每个包（实现 Routes() []httpx.Route）加入 domains，并从 contract_test.go 的 notYetImplemented 中删去对应路由；
//   - adminHooks.UserStatusChanged 接订阅包“用户状态变化 → 该用户全部订阅生成新期望版本”（封禁 ≤1 秒断开，R-13.8）；
//     adminHooks.UserSubscriptions / UserRecentOrders 接用户详情页的订阅与订单列表；
//   - agent 与 hub 接节点同步包的 agentproto.Server 实现；
//   - background 接调度循环（docs/06 §3，每 5 秒）。
func wireCore(d *Deps) (coreParts, error) {
	_ = d
	return coreParts{}, nil
}
