package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"
)

// LoggerName 是脱敏日志器在 GOST 日志器注册表中的名字；agent 创建的每个服务都用它（accessServiceConfigs）。
const LoggerName = "isp-redact"

// 诊断不泄露正文（R-07、R-13，A-18）。GOST 的处理器会把客户发来的内容写进日志：
//   - http：http.ReadRequest 的错误带请求行、头的原文（malformed HTTP request "…"、malformed MIME header line: "…"，
//     Go 标准库一律用 %q 引用）；trace 级别 DumpRequest 整个请求头，含 Proxy-Authorization（Basic 明文）；
//     字段 user 是客户发来的用户名（认证之前就写入，错误的用户名也会记下）。
//   - socks5：trace 级别打印用户名/密码请求（含明文密码）。
//
// 不 fork go-gost/x，而是给 agent 创建的服务配一个包装日志器：
//   - trace 一律只记长度与摘要（这一级在这些处理器里基本都是请求/认证报文的转储）；
//   - 其他级别把消息里的双引号字符串（%q 的产物）换成长度与摘要；去掉引用后仍含控制字符或非法 UTF-8 的整条换掉；
//   - 字段 user 换成摘要；其他字符串字段含控制字符、非法 UTF-8 或超过 128 字节时换成摘要。
//
// 地址、服务名、方向标记（<>、<->、>-<）、端口占用等错误原样保留。已知代价：caller 字段指向本文件。
type redactLogger struct {
	l logger.Logger // nil：调用时取 logger.Default()（GOST 在注册之后才设置默认日志器）
}

// redactBase 只供测试替换底层日志器（logger.SetDefault 不允许换成不同类型的实现）。
var redactBase atomic.Pointer[logger.Logger]

func newRedactLogger(base logger.Logger) logger.Logger { return redactLogger{l: base} }

func (r redactLogger) base() logger.Logger {
	if r.l != nil {
		return r.l
	}
	if p := redactBase.Load(); p != nil {
		return *p
	}
	if d := logger.Default(); d != nil {
		return d
	}
	return xlogger.Nop()
}

// digest 返回内容的长度与短摘要，便于关联同一内容而不泄露原文。
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("<已脱敏 %d 字节 sha256:%s>", len(s), hex.EncodeToString(sum[:4]))
}

var quotedRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func unsafeText(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' {
			return true
		}
	}
	return false
}

// redactMsg 脱敏一条非 trace 消息。
func redactMsg(s string) string {
	out := quotedRe.ReplaceAllStringFunc(s, func(q string) string { return digest(q[1 : len(q)-1]) })
	if unsafeText(out) {
		return digest(s)
	}
	return out
}

func redactFields(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		s, ok := v.(string)
		switch {
		case !ok:
			out[k] = v
		case k == "user":
			out[k] = digest(s)
		case unsafeText(s) || len(s) > 128 || strings.Contains(s, `"`):
			out[k] = digest(s)
		default:
			out[k] = s
		}
	}
	return out
}

func (r redactLogger) WithFields(m map[string]any) logger.Logger {
	return redactLogger{l: r.base().WithFields(redactFields(m))}
}

func (r redactLogger) trace(msg string) { r.base().Trace("trace " + digest(msg)) }

func (r redactLogger) Trace(args ...any) {
	if r.IsLevelEnabled(logger.TraceLevel) {
		r.trace(fmt.Sprint(args...))
	}
}

func (r redactLogger) Tracef(format string, args ...any) {
	if r.IsLevelEnabled(logger.TraceLevel) {
		r.trace(fmt.Sprintf(format, args...))
	}
}

func (r redactLogger) Debug(args ...any) {
	if r.IsLevelEnabled(logger.DebugLevel) {
		r.base().Debug(redactMsg(fmt.Sprint(args...)))
	}
}

func (r redactLogger) Debugf(format string, args ...any) {
	if r.IsLevelEnabled(logger.DebugLevel) {
		r.base().Debug(redactMsg(fmt.Sprintf(format, args...)))
	}
}

func (r redactLogger) Info(args ...any) { r.base().Info(redactMsg(fmt.Sprint(args...))) }
func (r redactLogger) Infof(format string, args ...any) {
	r.base().Info(redactMsg(fmt.Sprintf(format, args...)))
}
func (r redactLogger) Warn(args ...any) { r.base().Warn(redactMsg(fmt.Sprint(args...))) }
func (r redactLogger) Warnf(format string, args ...any) {
	r.base().Warn(redactMsg(fmt.Sprintf(format, args...)))
}
func (r redactLogger) Error(args ...any) { r.base().Error(redactMsg(fmt.Sprint(args...))) }
func (r redactLogger) Errorf(format string, args ...any) {
	r.base().Error(redactMsg(fmt.Sprintf(format, args...)))
}
func (r redactLogger) Fatal(args ...any) { r.base().Fatal(redactMsg(fmt.Sprint(args...))) }
func (r redactLogger) Fatalf(format string, args ...any) {
	r.base().Fatal(redactMsg(fmt.Sprintf(format, args...)))
}
func (r redactLogger) GetLevel() logger.LogLevel { return r.base().GetLevel() }
func (r redactLogger) IsLevelEnabled(level logger.LogLevel) bool {
	return r.base().IsLevelEnabled(level)
}
