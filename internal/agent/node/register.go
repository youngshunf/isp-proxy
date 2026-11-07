package node

// 运行时生成的接入服务与出口链用到的 GOST 组件。cmd/gost 的 register.go 已经导入它们；
// 这里再导入一次，让本包在测试与其他入口中也能独立工作（Go 的包初始化只执行一次，不会重复注册）。
// isp-tcp、isp-tls、isp-auth 由 agent.Install 在运行时启动时注册，不带 -agent-config 时 GOST 的注册表不变。
import (
	_ "github.com/go-gost/x/connector/http"
	_ "github.com/go-gost/x/connector/socks/v5"
	_ "github.com/go-gost/x/dialer/tcp"
	_ "github.com/go-gost/x/dialer/tls"
	_ "github.com/go-gost/x/handler/http"
	_ "github.com/go-gost/x/handler/socks/v5"
	_ "github.com/go-gost/x/listener/tcp"
	_ "github.com/go-gost/x/listener/tls"
)
