package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gost/core/chain"
)

type stubChain struct{ r chain.Route }

func (s stubChain) Route(context.Context, string, string, ...chain.RouteOption) chain.Route {
	return s.r
}

type emptyRoute struct{}

func (emptyRoute) Dial(context.Context, string, string, ...chain.DialOption) (net.Conn, error) {
	return nil, nil
}
func (emptyRoute) Bind(context.Context, string, string, ...chain.BindOption) (net.Listener, error) {
	return nil, nil
}
func (emptyRoute) Nodes() []*chain.Node { return nil }

// 出口链缺失、返回空路由或选不出节点时必须失败，不能交给 GOST 退化为直连。
func TestExitChainFailsClosed(t *testing.T) {
	cases := map[string]*exitChain{"unset": {}}
	for name, r := range map[string]chain.Route{"nil route": nil, "no nodes": emptyRoute{}} {
		ec := &exitChain{}
		ec.inner.Store(&chainHolder{c: stubChain{r: r}})
		cases[name] = ec
	}
	for name, ec := range cases {
		r := ec.Route(context.Background(), "tcp", "example.com:80")
		if r == nil || len(r.Nodes()) != 0 {
			t.Fatalf("%s: got %T", name, r)
		}
		if _, err := r.Dial(context.Background(), "tcp", "example.com:80"); !errors.Is(err, ErrNoExit) {
			t.Fatalf("%s: dial err %v", name, err)
		}
	}
}

func newBareAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// 下行授权在并发下严格等于额度，不多一字节。
func TestDownstreamGrantIsExact(t *testing.T) {
	a := newBareAgent(t)
	a.Upsert(SubState{ID: "s", Version: 1, EntitledTotal: 1_000_003, ExpiresAt: time.Now().Add(time.Hour), Enabled: true})
	s := a.lookup("s")

	var granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				g := s.grant(4096)
				if g == 0 {
					return
				}
				granted.Add(int64(g))
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 1_000_003 || a.Used("s") != 1_000_003 {
		t.Fatalf("granted %d used %d", granted.Load(), a.Used("s"))
	}
}

// 低版本状态被忽略；累计用量只增不减。
func TestUpsertVersionAndMonotonicUsage(t *testing.T) {
	a := newBareAgent(t)
	exp := time.Now().Add(time.Hour)
	a.Upsert(SubState{ID: "s", Version: 5, EntitledTotal: 100, ExpiresAt: exp, Enabled: true, UsedFloor: 40})
	if a.Upsert(SubState{ID: "s", Version: 4, EntitledTotal: 999, ExpiresAt: exp, Enabled: true}) {
		t.Fatal("stale version applied")
	}
	a.Upsert(SubState{ID: "s", Version: 6, EntitledTotal: 100, ExpiresAt: exp, Enabled: true, UsedFloor: 10})
	if a.Used("s") != 40 {
		t.Fatalf("used %d, want 40", a.Used("s"))
	}
	if a.lookup("s").entitled.Load() != 100 {
		t.Fatal("stale entitlement applied")
	}
}
