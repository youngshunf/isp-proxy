//go:build ispe2e

package isp

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 计量与停用：A-03～A-07、A-10、A-15、A-18、A-22。

// D-17 冻结值：透支 ≤ 1,000,000 字节，停用 ≤ 1 秒。
const (
	maxOvershoot = 1_000_000
	maxStopDelay = time.Second
)

// SettledUsed 等待订阅的上报用量稳定（连续 3 次读数相同，间隔大于上报周期），返回该值。
func (e *Env) SettledUsed(id string) int64 {
	e.t.Helper()
	var last int64 = -1
	same := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		u := e.Admin.Sub(id).Traffic.UsedBytes
		if u == last {
			same++
			if same >= 3 {
				return u
			}
		} else {
			same, last = 0, u
		}
		time.Sleep(600 * time.Millisecond)
	}
	e.t.Fatalf("订阅 %s 的用量 20 秒内未稳定（最后 %d）", id, last)
	return 0
}

// A-03 两个用户各两个订阅产生不同流量；四个订阅的累计用量与实际字节一致，互不串账。
func TestA03_PerSubscriptionUsageIsolated(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 4})
	users := []*User{e.NewUser(), e.NewUser()}
	type sub struct {
		u         *User
		s         Subscription
		body, raw int64
		want      int64
		mode      Mode
	}
	var subs []*sub
	for i := 0; i < 4; i++ {
		u := users[i/2]
		s := e.Provision(u, ProvisionOpts{})
		subs = append(subs, &sub{u: u, s: s, want: int64(i+1) * MB, mode: AllModes[i]})
	}
	base := map[string]int64{}
	for _, x := range subs {
		base[x.s.ID] = e.SettledUsed(x.s.ID) // 开通后的就绪检查也经入口访问过一次
	}
	var wg sync.WaitGroup
	for _, x := range subs {
		wg.Add(1)
		go func(x *sub) {
			defer wg.Done()
			got, raw, err := Download(x.mode, CredsOf(x.s.Access), e.Target(), x.want, 30*time.Second)
			if err != nil {
				t.Errorf("%s 下载：%v", x.s.ID, err)
			}
			x.body, x.raw = got, raw
		}(x)
	}
	wg.Wait()
	for _, x := range subs {
		used := e.SettledUsed(x.s.ID) - base[x.s.ID]
		// 计量的是认证后隧道上的应用层字节（含 HTTP 头、TLS 之上），所以 ≥ 正文、≤ TCP 层字节。
		if used < x.body || used > x.raw+4*KB {
			t.Errorf("订阅 %s（%s）用量 %d，正文 %d，TCP 层 %d：不一致", x.s.ID, x.mode, used, x.body, x.raw)
		}
		t.Logf("订阅 %s（%s）：正文 %d、TCP %d、计量 %d", x.s.ID, x.mode, x.body, x.raw, used)
	}
	// 用户只能看到自己的两个订阅。
	for i, u := range users {
		list := u.Subs()
		if len(list) != 2 {
			t.Fatalf("用户 %d 看到 %d 个订阅", i, len(list))
		}
		for _, s := range list {
			if s.ID != subs[2*i].s.ID && s.ID != subs[2*i+1].s.ID {
				t.Fatalf("用户 %d 看到了不属于自己的订阅 %s", i, s.ID)
			}
		}
	}
}

// A-04 同一订阅同时使用三种入口；合并用量等于三者之和，没有三份余额。
func TestA04_ThreeEntriesShareOneBalance(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	before := e.Admin.Sub(s.ID)
	base := e.SettledUsed(s.ID)
	var body, raw atomic.Int64
	var wg sync.WaitGroup
	for _, m := range AllModes {
		wg.Add(1)
		go func(m Mode) {
			defer wg.Done()
			got, r, err := Download(m, CredsOf(s.Access), e.Target(), 2*MB, 30*time.Second)
			if err != nil {
				t.Errorf("%s：%v", m, err)
			}
			body.Add(got)
			raw.Add(r)
		}(m)
	}
	wg.Wait()
	used := e.SettledUsed(s.ID) - base
	if used < body.Load() || used > raw.Load()+16*KB {
		t.Fatalf("合并用量 %d 应等于各入口之和（正文 %d、TCP 层 %d）", used, body.Load(), raw.Load())
	}
	after := e.Admin.Sub(s.ID)
	if after.Traffic.TotalBytes != before.Traffic.TotalBytes {
		t.Fatalf("流量合计不应变化：%d → %d", before.Traffic.TotalBytes, after.Traffic.TotalBytes)
	}
	if after.Traffic.RemainingBytes != after.Traffic.TotalBytes-after.Traffic.UsedBytes {
		t.Fatalf("只有一份余额：剩余 %d ≠ 合计 %d − 已用 %d", after.Traffic.RemainingBytes, after.Traffic.TotalBytes, after.Traffic.UsedBytes)
	}
}

// A-05 极小额度下建立双向大流量连接；used_total ≤ entitled_total（透支 ≤ 1,000,000 字节），新连接被明确拒绝，
// 存量停止，其他订阅正常，进程无空转。
func TestA05_TinyQuotaBidirectionalFlood(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	other := e.Provision(u, ProvisionOpts{})
	e.Admin.SetRemainingBytes(s.ID, 2*MB)
	e.WaitApplied(s.ID)
	c := CredsOf(s.Access)

	var recv atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for _, m := range AllModes {
		wg.Add(2)
		go func(m Mode) {
			defer wg.Done()
			errs <- Stream(m, c, e.Target(), 50*MB, &recv, 30*time.Second)
		}(m)
		go func(m Mode) {
			defer wg.Done()
			_, err := Upload(m, c, e.Target(), 50*MB, 30*time.Second)
			errs <- err
		}(m)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("耗尽后存量连接没有停止")
	}
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("有一条 50 MB 的传输在 2 MB 额度下完成了")
		}
	}
	sa := e.WaitAdminSub(s.ID, 10*time.Second, func(a *AdminSubscription) bool { return a.Has("traffic_exhausted") }, "耗尽阻断")
	used := e.SettledUsed(s.ID)
	sa = e.Admin.Sub(s.ID)
	if over := used - sa.Traffic.TotalBytes; over > maxOvershoot {
		t.Fatalf("透支 %d 字节 > %d（used %d、entitled %d）", over, maxOvershoot, used, sa.Traffic.TotalBytes)
	}
	if recv.Load() > 2*MB+maxOvershoot {
		t.Fatalf("客户端收到 %d 字节，超过剩余额度 + 1 MB", recv.Load())
	}
	e.MustReject(c, "耗尽后的新连接")
	us := u.Sub(s.ID)
	if !us.Has("traffic_exhausted") {
		t.Fatalf("用户视图应显示 traffic_exhausted：%v", us.Blockers)
	}
	// 其他订阅不受影响。
	e.MustReach(CredsOf(other.Access), e.CurrentExitLabel(other.ID))
	// 进程无空转：空闲 3 秒内 gost 的 CPU 时间增长 < 0.3 秒。
	time.Sleep(time.Second)
	c0 := e.Gost.cpuTime(t)
	time.Sleep(3 * time.Second)
	if d := e.Gost.cpuTime(t) - c0; d > 300*time.Millisecond {
		t.Fatalf("耗尽后 gost 空转：3 秒内 CPU %v", d)
	}
}

// A-06 同一订阅数十个并发连接争用最后的额度；总授权不超过净入账额度（透支 ≤ 1,000,000 字节）。
func TestA06_ConcurrentLastQuota(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	const remaining = 3 * MB
	e.Admin.SetRemainingBytes(s.ID, remaining)
	e.WaitApplied(s.ID)
	c := CredsOf(s.Access)
	var recv atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = Stream(TunnelModes[i%3], c, e.Target(), 2*MB, &recv, 30*time.Second)
		}(i)
	}
	wg.Wait()
	sa := e.WaitAdminSub(s.ID, 10*time.Second, func(a *AdminSubscription) bool { return a.Has("traffic_exhausted") }, "耗尽")
	used := e.SettledUsed(s.ID)
	if used > sa.Traffic.TotalBytes+maxOvershoot {
		t.Fatalf("并发争用后透支 %d 字节", used-sa.Traffic.TotalBytes)
	}
	if recv.Load() > remaining+maxOvershoot {
		t.Fatalf("48 条连接共收到 %d 字节，超过剩余 %d + 1 MB", recv.Load(), remaining)
	}
	t.Logf("剩余 %d，客户端共收到 %d，计量透支 %d", remaining, recv.Load(), used-sa.Traffic.TotalBytes)
}

// A-07 耗尽时已有的空闲隧道 ≤ 1 秒被主动关闭，之后无法恢复传输。
func TestA07_IdleTunnelsClosedOnExhaustion(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)
	var idle []*PConn
	for _, m := range AllModes {
		p, err := Open(m, c, e.Target())
		if err != nil {
			t.Fatalf("%s：%v", m, err)
		}
		defer p.Close()
		idle = append(idle, p)
	}
	e.Admin.SetRemainingBytes(s.ID, 1*MB)
	e.WaitApplied(s.ID)
	closedAt := make([]time.Time, len(idle))
	var wg sync.WaitGroup
	for i, p := range idle {
		wg.Add(1)
		go func(i int, p *PConn) {
			defer wg.Done()
			if _, ok := WaitClosed(p, 20*time.Second); ok {
				closedAt[i] = time.Now()
			}
		}(i, p)
	}
	var recv atomic.Int64
	err := Stream(Connect, c, e.Target(), 10*MB, &recv, 20*time.Second)
	exhaustedAt := time.Now()
	if err == nil {
		t.Fatal("10 MB 下载在 1 MB 额度下完成了")
	}
	wg.Wait()
	for i, at := range closedAt {
		if at.IsZero() {
			t.Fatalf("%s 空闲隧道在耗尽后没有被关闭", AllModes[i])
		}
		if d := at.Sub(exhaustedAt); d > maxStopDelay {
			t.Fatalf("%s 空闲隧道在耗尽后 %v 才关闭（> 1 秒）", AllModes[i], d)
		}
	}
	// 关闭后不能恢复传输。
	for i, p := range idle {
		if _, err := p.Get("/ip"); err == nil {
			t.Fatalf("%s 空闲隧道关闭后仍能传输", AllModes[i])
		}
	}
	e.MustReject(c, "耗尽后")
}

// A-10 负载中 kill -9 后重启，并重放上报；累计用量不回退，少计 ≤ 1 秒用量，无重复扣量。
func TestA10_Kill9UnderLoadMonotonicUsage(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1, Plan: func(p *PlanVersionInput) {
		p.BandwidthMbps = 160 // 20 MB/s：让“1 秒用量”有确定的上界
		p.IncludedBytes = 2 * GB
	}})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)

	// 后台持续读取 API 上的用量，验证单调。
	var mono sync.Mutex
	var observed []int64
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stopPoll:
				return
			case <-time.After(150 * time.Millisecond):
			}
			var a AdminSubscription
			if _, err := e.Admin.Do("GET", "/subscriptions/"+s.ID, nil, &a); err == nil {
				mono.Lock()
				observed = append(observed, a.Traffic.UsedBytes)
				mono.Unlock()
			}
		}
	}()
	base := e.SettledUsed(s.ID)

	var recv atomic.Int64
	for i := 0; i < 4; i++ {
		go func(m Mode) { _ = Stream(m, c, e.Target(), 1*GB, &recv, 60*time.Second) }(AllModes[i])
	}
	time.Sleep(3 * time.Second)
	r0 := recv.Load()
	time.Sleep(time.Second)
	rate := recv.Load() - r0 // 最后 1 秒的字节
	e.Gost.kill9(t)
	atKill := recv.Load()
	t.Logf("kill -9 时客户端已收到 %d 字节（最后 1 秒 %d 字节）", atKill-0, rate)

	e.RestartGost()
	e.WaitNodeOnline()
	after := e.SettledUsed(s.ID) - base
	close(stopPoll)
	<-pollDone
	mono.Lock()
	for i := 1; i < len(observed); i++ {
		if observed[i] < observed[i-1] {
			t.Fatalf("累计用量回退：%d → %d", observed[i-1], observed[i])
		}
	}
	mono.Unlock()
	if lost := atKill - after; lost > rate+64*KB {
		t.Fatalf("少计 %d 字节，超过 1 秒用量 %d", lost, rate)
	}
	if after > atKill+64*KB {
		t.Fatalf("重复计量：计量 %d > 客户端收到 %d", after, atKill)
	}
	// 重放：再重启一次 gost、重启控制面（节点重放上报），用量不变。
	e.Gost.kill9(t)
	e.RestartGost()
	e.StopControl()
	e.StartControl()
	e.WaitNodeOnline()
	if again := e.SettledUsed(s.ID) - base; again != after {
		t.Fatalf("重放上报后用量变化：%d → %d", after, again)
	}
	// 重启后订阅仍可用（未耗尽）。
	e.MustReach(c, e.CurrentExitLabel(s.ID), Connect)
}

// A-15 出口检测流量单独记录，不扣客户额度；客户无法伪造内部检测身份。
func TestA15_ProbesNotChargedNoForgedIdentity(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	used := e.SettledUsed(s.ID)
	for i := 0; i < 4; i++ {
		var p Probe
		e.Admin.Must("POST", "/subscriptions/"+s.ID+"/exit-checks", nil, &p)
		if p = e.Admin.WaitProbe(p.ID); p.Status != "done" || deref(p.ObservedIP) != e.Exits[0].IP {
			t.Fatalf("出口检测：%+v", p)
		}
	}
	if _, err := e.Admin.CompareCandidate(s.ID, e.Exits[1].ResourceID); err != nil {
		t.Fatal(err)
	}
	if got := e.SettledUsed(s.ID); got != used {
		t.Fatalf("检测流量计入了客户用量：%d → %d", used, got)
	}
	// 伪造检测身份：常见的内部用户名、空凭据、上游凭据都不能经客户入口使用。
	c := CredsOf(s.Access)
	for _, fake := range []Creds{{User: "probe", Pass: ""}, {User: "", Pass: ""}, {User: "__probe__", Pass: "probe"},
		{User: e.Exits[0].Up.User, Pass: e.Exits[0].Up.Pass}, {User: e.NodeID, Pass: ""}} {
		fake.Host, fake.Ports = c.Host, c.Ports
		e.MustReject(fake, "伪造身份 "+fake.User)
	}
	if got := e.SettledUsed(s.ID); got != used {
		t.Fatalf("伪造身份的连接计入了用量：%d → %d", used, got)
	}
	if e.CanaryHits() != 0 {
		t.Fatal("出现直连")
	}
}

// A-18 错误密码和垃圾数据冲击端口时，订阅用量不变；诊断不泄露密码和正文。
func TestA18_WrongPasswordAndJunkNotMetered(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)
	used := e.SettledUsed(s.ID)
	wrongPass := "WRONGPASS" + randHex(6)
	junkMark := "JUNKMARK" + randHex(6)
	junk := []byte(strings.Repeat(junkMark+"\x00\xff\r\n", 1<<14)) // ≈ 400 KB
	var wg sync.WaitGroup
	for _, m := range AllModes {
		for i := 0; i < 20; i++ {
			wg.Add(2)
			go func(m Mode) {
				defer wg.Done()
				_, _, _ = Fetch(m, c.WithPass(wrongPass), e.Target(), "/bytes?n=100000")
			}(m)
			go func(m Mode) {
				defer wg.Done()
				conn, err := net.DialTimeout("tcp", c.Addr(m), 2*time.Second)
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				// 首行总以完整标记开头（GOST 只记录首个请求行），其后按随机偏移错位，覆盖各种截断。
				off := rand.IntN(64)
				_, _ = conn.Write(append([]byte(junkMark), junk[off:]...))
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}(m)
		}
	}
	wg.Wait()
	if got := e.SettledUsed(s.ID); got != used {
		t.Fatalf("错误密码与垃圾数据计入了用量：%d → %d", used, got)
	}
	e.MustReach(c, e.CurrentExitLabel(s.ID))
	for _, f := range []string{e.Gost.logPath, e.Control.logPath} {
		b, _ := os.ReadFile(f)
		if leak := containsAny(string(b), wrongPass, c.Pass); leak != "" {
			t.Fatalf("%s 泄露了密码 %q", filepath.Base(f), leak)
		}
		if i := strings.Index(string(b), junkMark); i >= 0 {
			// GOST 处理器会把畸形请求行原样写进错误日志；agent 服务走脱敏日志器 isp-redact（internal/agent/redact.go）。
			t.Fatalf("%s 记录了客户发送的原始数据：…%s…", filepath.Base(f), truncate(string(b[max(0, i-200):]), 300))
		}
	}
	// 脱敏不是靠“不记日志”：畸形请求仍有错误日志，只是内容换成了长度与摘要。
	if b, _ := os.ReadFile(e.Gost.logPath); !strings.Contains(string(b), "malformed HTTP request") || !strings.Contains(string(b), "已脱敏") {
		t.Fatal("gost.log 应保留畸形请求的诊断（脱敏后）")
	}
}

// A-22 从旧备份恢复控制面时，累计用量取备份与节点本地的较大值，不重新开放已消费流量。
func TestA22_RestoreOlderBackupKeepsMaxUsage(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("找不到 %s：%v", bin, err)
		}
	}
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)
	e.Admin.SetRemainingBytes(s.ID, 20*MB)
	e.WaitApplied(s.ID)
	if _, _, err := Download(Connect, c, e.Target(), 5*MB, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	usedAtBackup := e.SettledUsed(s.ID)

	// 按运行手册（deploy/backup.sh）做一次备份。
	root, _ := filepath.Abs("../..")
	backups := filepath.Join(e.Dir, "backups")
	cmd := exec.Command("bash", filepath.Join(root, "deploy", "backup.sh"), backups)
	cmd.Env = append(os.Environ(), "ISP_DATABASE_URL="+e.DBURL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy/backup.sh 退出码非 0：%v\n%s", err, out)
	}
	dumps, _ := filepath.Glob(filepath.Join(backups, "isp-control-*.dump"))
	if len(dumps) != 1 {
		t.Fatalf("备份文件：%v", dumps)
	}

	// 备份之后继续消费到耗尽。
	var recv atomic.Int64
	_ = Stream(Connect, c, e.Target(), 40*MB, &recv, 30*time.Second)
	e.WaitAdminSub(s.ID, 10*time.Second, func(a *AdminSubscription) bool { return a.Has("traffic_exhausted") }, "耗尽")
	usedAtFailure := e.SettledUsed(s.ID)
	if usedAtFailure <= usedAtBackup {
		t.Fatalf("备份后应有更多用量：%d ≤ %d", usedAtFailure, usedAtBackup)
	}

	// 控制面故障：停止 → 按运行手册从旧备份恢复 → 启动。
	e.StopControl()
	rcmd := exec.Command("pg_restore", "--clean", "--if-exists", "--no-owner", "-d", e.DBURL, dumps[0])
	if out, err := rcmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_restore：%v\n%s", err, out)
	}
	e.StartControl()
	// 恢复瞬间库里是旧值；在节点上报合并之前和之后，已消费的流量都不能重新开放。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := e.Reach(Connect, c); err == nil {
			t.Fatalf("从旧备份恢复后已耗尽的订阅被重新开放（到达 %q）", body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.WaitNodeOnline()
	sa := e.WaitAdminSub(s.ID, 20*time.Second, func(a *AdminSubscription) bool { return a.Traffic.UsedBytes >= usedAtFailure },
		"用量恢复为节点本地的较大值")
	if !sa.Has("traffic_exhausted") {
		t.Fatalf("恢复后应仍显示耗尽：%v", sa.Blockers)
	}
	e.MustReject(c, "恢复后")
	t.Logf("备份时 %d，故障前 %d，恢复后 %d", usedAtBackup, usedAtFailure, sa.Traffic.UsedBytes)
	_ = fmt.Sprint()
}
