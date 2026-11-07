//go:build ispe2e

package isp

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// proc 是一个子进程（isp-control 或 gost）。stdout/stderr 追加写入日志文件，重启时继续追加，
// 测试失败时由 Env 的清理函数打印日志尾部。
type proc struct {
	name    string
	bin     string
	args    []string
	env     []string
	logPath string

	cmd  *exec.Cmd
	logf *os.File
	done chan struct{}
	err  error
}

func newProc(name, logPath, bin string, env []string, args ...string) *proc {
	return &proc{name: name, bin: bin, args: args, env: env, logPath: logPath}
}

// start 启动（或重启）进程。
func (p *proc) start(t testing.TB) {
	t.Helper()
	f, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n===== %s 启动 %s =====\n", p.name, time.Now().Format(time.RFC3339Nano))
	cmd := exec.Command(p.bin, p.args...)
	cmd.Env = append(os.Environ(), p.env...)
	cmd.Stdout = f
	cmd.Stderr = f
	// 独立进程组：测试被中断时不会把信号传给子进程之外的东西，kill -9 也只打到它自己。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatalf("启动 %s: %v", p.name, err)
	}
	p.cmd, p.logf, p.done, p.err = cmd, f, make(chan struct{}), nil
	procMu.Lock()
	allProc[p] = true
	procMu.Unlock()
	go func(cmd *exec.Cmd, done chan struct{}, f *os.File) {
		p.err = cmd.Wait()
		fmt.Fprintf(f, "===== %s 退出 %v %s =====\n", p.name, p.err, time.Now().Format(time.RFC3339Nano))
		f.Close()
		close(done)
	}(cmd, p.done, f)
}

func (p *proc) running() bool {
	if p.done == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// kill9 发送 SIGKILL 并等待退出（A-10 的真实崩溃）。
func (p *proc) kill9(t testing.TB) {
	t.Helper()
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s 在 SIGKILL 后未退出", p.name)
	}
}

// stop 发送 SIGTERM，10 秒内未退出则 SIGKILL。
func (p *proc) stop() {
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.done
	}
	procMu.Lock()
	delete(allProc, p)
	procMu.Unlock()
}

// pid 返回当前进程号（未运行时为 0）。
func (p *proc) pid() int {
	if !p.running() {
		return 0
	}
	return p.cmd.Process.Pid
}

// cpuTime 返回进程累计 CPU 时间（ps -o cputime，精度 1 秒以内取决于平台；macOS 输出 M:SS.ss）。
func (p *proc) cpuTime(t testing.TB) time.Duration {
	t.Helper()
	out, err := exec.Command("ps", "-o", "cputime=", "-p", strconv.Itoa(p.pid())).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	return parseCPUTime(strings.TrimSpace(string(out)))
}

// parseCPUTime 解析 [[DD-]HH:]MM:SS[.ss]。
func parseCPUTime(s string) time.Duration {
	var days int
	if i := strings.Index(s, "-"); i >= 0 {
		days, _ = strconv.Atoi(s[:i])
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var d time.Duration
	for i, part := range parts {
		v, _ := strconv.ParseFloat(part, 64)
		pow := len(parts) - 1 - i
		mult := time.Second
		for j := 0; j < pow; j++ {
			mult *= 60
		}
		d += time.Duration(v * float64(mult))
	}
	return d + time.Duration(days)*24*time.Hour
}

// tail 返回日志最后 n 行。
func tailFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return "(无日志: " + err.Error() + ")"
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}
