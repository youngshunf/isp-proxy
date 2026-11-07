package agent

// A-W1：真实进程崩溃（A-10、A-22）。子进程是本测试二进制自身（TestMain 识别环境变量后
// 转入 crashChild），在真实 GOST 服务上承载持续流量时被 SIGKILL，随后用同一数据文件重启。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const crashChildEnv = "ISP_AGENT_CRASH_CHILD"

type crashChildConfig struct {
	Store        string
	Flush        time.Duration
	Access       Access
	Exit         Exit
	BandwidthBps int64
	// Floors 依次作为 UsedFloor 下发（版本递增），模拟控制面在重启后重放、乱序下发已收到的上报。
	Floors []uint64
}

// crashChild 是子进程的主函数：开通订阅 s1 后每 10 ms 在标准输出打印一次累计用量，直到被杀。
// 输出协议：“restored N”（从数据文件恢复的值）、“floored N”（应用 UsedFloor 后的值）、
// “ready”、之后是若干“used N”。
func crashChild() int {
	var cfg crashChildConfig
	if err := json.Unmarshal([]byte(os.Getenv(crashChildEnv)), &cfg); err != nil {
		fmt.Println("error", err)
		return 2
	}
	a, err := New(Options{Store: FileStore{Path: cfg.Store}, FlushEvery: cfg.Flush, Egress: loopbackEgress})
	if err != nil {
		fmt.Println("error", err)
		return 2
	}
	tlsCfg, err := genSelfSignedTLS()
	if err != nil {
		fmt.Println("error", err)
		return 2
	}
	Install(a, tlsCfg)
	fmt.Printf("restored %d\n", a.Used("s1"))
	exp := time.Now().Add(time.Hour)
	floors := append([]uint64{0}, cfg.Floors...)
	for i, f := range floors {
		a.Upsert(SubState{ID: "s1", Version: uint64(i + 1), EntitledTotal: 1 << 50, ExpiresAt: exp, Enabled: true, UsedFloor: f, BandwidthBps: cfg.BandwidthBps})
	}
	fmt.Printf("floored %d\n", a.Used("s1"))
	if err := a.ApplyAccess("s1", cfg.Access, &cfg.Exit); err != nil {
		fmt.Println("error", err)
		return 2
	}
	fmt.Println("ready")
	for {
		time.Sleep(10 * time.Millisecond)
		fmt.Printf("used %d\n", a.Used("s1"))
	}
}

type crashReport struct {
	at   time.Time // 父进程收到的时间，不早于子进程打印的时间
	used uint64
}

type crashProc struct {
	cmd      *exec.Cmd
	restored uint64
	floored  uint64
	mu       sync.Mutex
	reports  []crashReport
	eof      chan struct{}
}

func startCrashChild(t *testing.T, cfg crashChildConfig) *crashProc {
	t.Helper()
	b, _ := json.Marshal(cfg)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), crashChildEnv+"="+string(b))
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &crashProc{cmd: cmd, eof: make(chan struct{})}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	sc := bufio.NewScanner(out)
	ready := make(chan error, 1)
	go func() {
		defer close(p.eof)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 0 {
				continue
			}
			var n uint64
			if len(f) > 1 {
				n, _ = strconv.ParseUint(f[1], 10, 64)
			}
			switch f[0] {
			case "restored":
				p.restored = n
			case "floored":
				p.floored = n
			case "ready":
				ready <- nil
			case "used":
				p.mu.Lock()
				p.reports = append(p.reports, crashReport{time.Now(), n})
				p.mu.Unlock()
			default:
				ready <- fmt.Errorf("child: %s", sc.Text())
			}
		}
		ready <- io.ErrUnexpectedEOF
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child not ready")
	}
	return p
}

// kill 发送 SIGKILL 并等待退出，返回发送时刻与此前收到的全部上报。
func (p *crashProc) kill(t *testing.T) (time.Time, []crashReport) {
	t.Helper()
	at := time.Now()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	p.cmd.Wait()
	<-p.eof
	p.mu.Lock()
	defer p.mu.Unlock()
	return at, p.reports
}

// load 驱动 n 条隧道持续下载，直到隧道被关闭；返回客户端实际收发的应用层字节数。
func crashLoad(acc Access, target string, n int) (stop func() int64) {
	var total atomic.Int64
	var wg sync.WaitGroup
	quit := make(chan struct{})
	for i := 0; i < n; i++ {
		p := protos[i%len(protos)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-quit:
					return
				default:
				}
				c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, target)
				if err != nil {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				c.SetDeadline(time.Now().Add(5 * time.Second))
				get(c, "/bytes?n=2000000000") // 一直读到连接断开（子进程被杀）
				total.Add(c.read.Load() + c.wrote.Load())
				c.Close()
			}
		}()
	}
	return func() int64 { close(quit); wg.Wait(); return total.Load() }
}

func TestRealCrashKill9(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL semantics are Unix-only")
	}
	const (
		flush = 200 * time.Millisecond
		// slack 覆盖 fsync、调度与管道延迟。损失窗口的断言是 flush + slack。
		slack = 150 * time.Millisecond
		bps   = 64 << 20
	)
	backend := startBackend(t, "exit-C")
	up := startUpstream(t, backend.addr, "uc", "pc")
	exit := Exit{Version: 1, Protocol: "http", Addr: up.addr, Username: "uc", Password: "pc"}
	store := filepath.Join(t.TempDir(), "used.json")
	newAcc := func(v uint64) Access {
		return Access{Version: v, Username: "u-crash", Password: "pw-crash",
			HTTPAddr: freeAddr(t), HTTPSAddr: freeAddr(t), SOCKS5Addr: freeAddr(t)}
	}
	onDisk := func() uint64 {
		m, err := FileStore{Path: store}.Load() // 每次都必须是完整、可解析的文件
		if err != nil {
			t.Fatalf("store unreadable after kill: %v", err)
		}
		return m["s1"]
	}

	var persisted, maxReported uint64
	for cycle := 1; cycle <= 2; cycle++ {
		acc := newAcc(uint64(cycle))
		p := startCrashChild(t, crashChildConfig{Store: store, Flush: flush, Access: acc, Exit: exit, BandwidthBps: bps})
		if p.restored != persisted {
			t.Fatalf("cycle %d: restored %d, on disk %d", cycle, p.restored, persisted)
		}
		stop := crashLoad(acc, backend.addr, 4)
		// 随机化 kill 相对持久化周期的相位。
		time.Sleep(600*time.Millisecond + rand.N(flush))
		killedAt, reports := p.kill(t)
		client := stop()

		if len(reports) < 20 {
			t.Fatalf("cycle %d: only %d reports", cycle, len(reports))
		}
		got := onDisk()
		last := reports[len(reports)-1].used
		maxReported = max(maxReported, last)
		// 不回退：落盘值不低于重启前的值，也不低于 kill 前 flush+slack 时已上报的任何值。
		if got < persisted {
			t.Fatalf("cycle %d: used_total regressed %d -> %d", cycle, persisted, got)
		}
		var bound uint64
		var lostSince time.Time
		for _, r := range reports {
			if !r.at.After(killedAt.Add(-flush - slack)) {
				bound = max(bound, r.used)
			}
			if r.used <= got {
				lostSince = r.at
			}
		}
		if got < bound {
			t.Fatalf("cycle %d: on disk %d < %d reported %v before kill (more than one flush lost)", cycle, got, bound, flush+slack)
		}
		// 只少计、不多扣：本轮新增的落盘值不超过客户端本轮实际收发的字节（加握手与在途余量）。
		if inflight := int64(1 << 20); int64(got-persisted) > client+inflight {
			t.Fatalf("cycle %d: on disk grew %d, exceeds client bytes %d + %d", cycle, got-persisted, client, inflight)
		}
		lost := int64(last) - int64(got)
		t.Logf("cycle %d: reports=%d last_reported=%d on_disk=%d lost=%d bytes (%.0f ms of traffic) client=%d",
			cycle, len(reports), last, got, max(lost, 0), killedAt.Sub(lostSince).Seconds()*1000, client)
		persisted = got
	}

	// 重启后控制面重放、乱序下发已收到的上报作为 UsedFloor：结果等于两者最大值，重复下发不再变化。
	acc := newAcc(9)
	floors := []uint64{maxReported, maxReported - 1000, 0, maxReported, persisted}
	p := startCrashChild(t, crashChildConfig{Store: store, Flush: flush, Access: acc, Exit: exit, BandwidthBps: bps, Floors: floors})
	if p.restored != persisted {
		t.Fatalf("restored %d, on disk %d", p.restored, persisted)
	}
	if want := max(persisted, maxReported); p.floored != want {
		t.Fatalf("after replayed floors used %d, want %d", p.floored, want)
	}
	stop := crashLoad(acc, backend.addr, 2)
	time.Sleep(300 * time.Millisecond)
	_, reports := p.kill(t)
	stop()
	for _, r := range reports {
		if r.used < p.floored {
			t.Fatalf("report %d below applied floor %d", r.used, p.floored)
		}
	}
	if got := onDisk(); got < p.floored {
		t.Fatalf("floor not persisted: on disk %d < %d", got, p.floored)
	}
}
