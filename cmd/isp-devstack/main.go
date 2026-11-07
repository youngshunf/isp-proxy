// Command isp-devstack 一条命令起本机开发栈（docs/05 §6「本地开发栈」）：
//
//	go run ./cmd/isp-devstack            # 默认 isp-control 监听 127.0.0.1:8080，状态目录 var/devstack
//
// 依次启动并接好：一次性 PostgreSQL（本机 initdb/pg_ctl，数据放在状态目录）、开发证书、isp-control（邮件文件投递）、
// seed-dev 种子数据、一个已绑定 TOTP 的管理员、若干假 ISP 上游与 IP 回显端点（internal/agent/agenttest）、
// 带 -agent-config 的 gost 节点（出站放行 127.0.0.0/8），并把上游录入库存、验证为可售。
// 打印前台/后台地址、账号、TOTP 密钥与邮件目录，同时写 <dir>/state.json 供浏览器 E2E（web/e2e）读取。
// Ctrl-C（或 SIGTERM）停止全部进程并删除状态目录（-keep 保留）。
//
// 只监听本机回环，不连接真实 ISP、不发真实邮件、不碰真实收款。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type options struct {
	dir        string
	listen     string
	portStart  int
	portCount  int
	keep       bool
	portalURL  string
	adminURL   string
	adminEmail string
	adminPass  string
	userEmail  string
	userPass   string
	quiet      bool
}

func main() {
	var o options
	flag.StringVar(&o.dir, "dir", "var/devstack", "状态目录（数据库、证书、邮件、日志；默认退出时删除）")
	flag.StringVar(&o.listen, "listen", "127.0.0.1:8080", "isp-control 对外 API 监听地址（两个前端的 Vite 代理默认指向 :8080）")
	flag.IntVar(&o.portStart, "port-start", 21000, "节点接入端口池起点（每个订阅占 3 个）")
	flag.IntVar(&o.portCount, "port-count", 300, "节点接入端口池大小")
	flag.BoolVar(&o.keep, "keep", false, "退出时保留状态目录")
	flag.StringVar(&o.portalURL, "portal-url", "http://127.0.0.1:5173", "用户前台地址（只用于打印与邮件里的链接）")
	flag.StringVar(&o.adminURL, "admin-url", "http://127.0.0.1:5174", "管理后台地址（只用于打印）")
	flag.StringVar(&o.adminEmail, "admin-email", "admin@dev.test", "开发管理员邮箱")
	flag.StringVar(&o.adminPass, "admin-password", "dev-admin-password", "开发管理员密码（至少 12 位，仅开发）")
	flag.StringVar(&o.userEmail, "user-email", "dev@example.com", "seed-dev 建立的已验证开发用户")
	flag.StringVar(&o.userPass, "user-password", "dev-password-123", "开发用户密码（仅开发）")
	flag.BoolVar(&o.quiet, "quiet", false, "只打印就绪摘要")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "isp-devstack:", err)
		os.Exit(1)
	}
}

// State 是写给浏览器 E2E 的就绪信息（<dir>/state.json）。
type State struct {
	ControlURL  string     `json:"controlUrl"`
	PortalURL   string     `json:"portalUrl"`
	AdminURL    string     `json:"adminUrl"`
	MailDir     string     `json:"mailDir"`
	Dir         string     `json:"dir"`
	Admin       Credential `json:"admin"`
	User        Credential `json:"user"`
	TOTPSecret  string     `json:"totpSecret"`
	TOTPLastUse int64      `json:"totpLastStep"`
	NodeID      string     `json:"nodeId"`
	NodeHost    string     `json:"nodeHost"`
	Exits       []ExitInfo `json:"exits"`
	Spares      []ExitInfo `json:"spares"`
	// ProbeURL 是经接入访问的目标：假上游不管目标地址，一律连到自己的回显服务，/ip 返回出口标签。
	// 用 127.0.0.1 上的地址是因为节点的出口目标白名单只放行 127.0.0.0/8（域名可能被本机 DNS 解析到别处）。
	ProbeURL string `json:"probeUrl"`
	PID      int    `json:"pid"`
}

// Credential 是一组登录凭据。
type Credential struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ExitInfo 是一个假出口：经它访问任何目标，目标侧看到的出口标签都是 Label（/ip 返回），回显 IP 为 IP。
type ExitInfo struct {
	Label      string `json:"label"`
	IP         string `json:"ip"`
	Country    string `json:"country"`
	City       string `json:"city"`
	URL        string `json:"url"` // 录入用的上游地址（含凭据）
	ResourceID string `json:"resourceId,omitempty"`
}

func run(ctx context.Context, o options) (err error) {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	dir := o.dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "state.json")); statErr == nil {
		if pid := readPID(filepath.Join(dir, "state.json")); pid > 0 && syscall.Kill(pid, 0) == nil {
			return fmt.Errorf("状态目录 %s 正被进程 %d 使用；换一个 -dir 或先停止它", dir, pid)
		}
	}
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st := newStack(dir, o)
	defer func() {
		st.teardown()
		if !o.keep {
			_ = os.RemoveAll(dir)
		} else {
			st.logf("状态目录已保留：%s", dir)
		}
	}()
	if err := st.bootstrap(ctx, root); err != nil {
		return err
	}
	st.printSummary()
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\n收到退出信号，正在停止开发栈……")
	return nil
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var s struct {
		PID int `json:"pid"`
	}
	_ = json.Unmarshal(b, &s)
	return s.PID
}

// repoRoot 找到含 go.mod 的仓库根（允许从子目录 go run）。
func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/go-gost/gost") {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("找不到仓库根目录（go.mod）；请在仓库内运行")
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// proc 是一个子进程，输出写入日志文件。
type proc struct {
	name string
	cmd  *exec.Cmd
	log  string
	done chan struct{}
}

func startProc(name, logPath string, env []string, bin string, args ...string) (*proc, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	// 独立进程组：终端里的 Ctrl-C 只发给本进程，由本进程按顺序停止子进程。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("启动 %s: %w", name, err)
	}
	p := &proc{name: name, cmd: cmd, log: logPath, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		f.Close()
		close(p.done)
	}()
	return p, nil
}

func (p *proc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *proc) stop() {
	if p == nil || !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.done
	}
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(无日志)"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func runCmd(env []string, out io.Writer, bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	b, err := cmd.CombinedOutput()
	if out != nil {
		_, _ = out.Write(b)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, b)
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
