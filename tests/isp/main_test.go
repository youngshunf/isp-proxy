//go:build ispe2e

package isp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/store/pgtest"
)

// 二进制只构建一次（TestMain），所有场景共用。
var bins struct {
	dir, control, gost string
}

func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "isp-e2e-bin-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		root, err := filepath.Abs("../..")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		bins.dir = dir
		bins.control = filepath.Join(dir, "isp-control")
		bins.gost = filepath.Join(dir, "gost")
		start := time.Now()
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, b := range []struct{ out, pkg string }{{bins.control, "./cmd/isp-control"}, {bins.gost, "./cmd/gost"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					errs[i] = fmt.Errorf("go build %s: %v\n%s", b.pkg, err, out)
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		fmt.Fprintf(os.Stderr, "isp e2e: 已构建 isp-control 与 gost（%.1fs）\n", time.Since(start).Seconds())
		defer pgtest.Shutdown()
		code := m.Run()
		killAllProcs()
		return code
	}()
	os.Exit(code)
}

// 所有启动过的子进程，测试二进制退出前兜底杀掉（正常情况下各自的 t.Cleanup 已停止）。
var (
	procMu  sync.Mutex
	allProc = map[*proc]bool{}
)

func killAllProcs() {
	procMu.Lock()
	defer procMu.Unlock()
	for p := range allProc {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGKILL)
		}
	}
}
