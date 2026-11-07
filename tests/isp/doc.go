// Package isp 是 ISP 代理平台的平台 E2E（docs/06 §6、docs/05 §7）：一次性 PostgreSQL、真实的 isp-control 与带
// agent 的 gost 二进制、本机回环的假 ISP 上游与 IP 回显端点，按 docs/02 §3 的 A-xx 编号逐项断言。
//
// 测试文件带构建标签 ispe2e，默认不参与 go test ./...：
//
//	go test -tags ispe2e ./tests/isp/... -count=1
//
// 只使用 127.0.0.1，不连接真实 ISP、不发真实邮件、不碰真实收款。
package isp
