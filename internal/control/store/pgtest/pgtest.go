// Package pgtest 为数据库测试提供一次性 PostgreSQL（docs/06 §6）。
//
// 数据源选择：
//   - 设置了 ISP_TEST_DATABASE_URL：连到该实例（账号需有 CREATEDB 权限），每个测试新建并在结束时删除一个数据库；
//     该实例连不上时测试失败而不是跳过。
//   - 否则：用本机 initdb + pg_ctl 在临时目录起一个只监听 127.0.0.1 随机端口的实例，整个测试二进制共享，
//     每个测试一个新数据库。找不到 PostgreSQL 程序时跳过并打印原因；设置 ISP_TEST_REQUIRE_DB=1 时改为失败（CI 用）。
//
// 用法：
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }   // 结束时停止共享实例
//
//	func TestX(t *testing.T) {
//	    pool := pgtest.New(t)        // 已执行全部迁移的新数据库
//	    url := pgtest.NewDatabase(t) // 空数据库（测迁移本身）
//	}
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL 指定外部测试实例的连接串。
const EnvURL = "ISP_TEST_DATABASE_URL"

// EnvRequire 为 1 时，找不到 PostgreSQL 视为失败而不是跳过。
const EnvRequire = "ISP_TEST_REQUIRE_DB"

type server struct {
	adminURL string // 连到 postgres 库、可建库的连接串
	external bool
	dir      string // 本地实例的临时目录
	pgCtl    string
	skip     string // 非空表示不可用的原因（跳过）
	err      error  // 配置了但启动/连接失败（失败）
}

var (
	once   sync.Once
	shared *server
)

func get() *server {
	once.Do(func() { shared = start() })
	return shared
}

func start() *server {
	if u := os.Getenv(EnvURL); u != "" {
		s := &server{adminURL: u, external: true}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, u)
		if err != nil {
			s.err = fmt.Errorf("pgtest: 连接 %s 失败: %w", EnvURL, err)
			return s
		}
		_ = c.Close(ctx)
		return s
	}
	initdb, err1 := exec.LookPath("initdb")
	pgCtl, err2 := exec.LookPath("pg_ctl")
	if err1 != nil || err2 != nil {
		return &server{skip: "pgtest: 未设置 " + EnvURL + "，且 PATH 中找不到 initdb/pg_ctl（需要 PostgreSQL 16 程序，例如 brew install postgresql@16）"}
	}
	dir, err := os.MkdirTemp("", "isp-pgtest-")
	if err != nil {
		return &server{err: err}
	}
	s := &server{dir: dir, pgCtl: pgCtl}
	data := filepath.Join(dir, "data")
	out, err := exec.Command(initdb, "-D", data, "-U", "postgres", "-A", "trust", "-E", "UTF8", "--locale=C", "-N").CombinedOutput()
	if err != nil {
		s.err = fmt.Errorf("pgtest: initdb 失败: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		return s
	}
	// 端口可能在选出与监听之间被占用，重试几次。
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		port, err := freePort()
		if err != nil {
			lastErr = err
			continue
		}
		opts := strings.Join([]string{
			"-p " + strconv.Itoa(port),
			"-c listen_addresses=127.0.0.1",
			"-c unix_socket_directories=''",
			"-c fsync=off",
			"-c synchronous_commit=off",
			"-c full_page_writes=off",
			"-c max_connections=200",
		}, " ")
		out, err := exec.Command(pgCtl, "-D", data, "-l", filepath.Join(dir, "postgres.log"), "-w", "-t", "60", "-o", opts, "start").CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("pg_ctl start: %v\n%s", err, out)
			continue
		}
		s.adminURL = fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
		return s
	}
	log, _ := os.ReadFile(filepath.Join(dir, "postgres.log"))
	s.err = fmt.Errorf("pgtest: 启动 PostgreSQL 失败: %v\n%s", lastErr, log)
	_ = os.RemoveAll(dir)
	return s
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// stop 停止本地共享实例并删除临时目录；外部实例不处理。
func stop() {
	if shared == nil || shared.external || shared.dir == "" || shared.adminURL == "" {
		return
	}
	_ = exec.Command(shared.pgCtl, "-D", filepath.Join(shared.dir, "data"), "-m", "immediate", "-w", "stop").Run()
	_ = os.RemoveAll(shared.dir)
}

// Main 运行测试，结束后停止共享实例。在 TestMain 中调用：func TestMain(m *testing.M) { pgtest.Main(m) }。
func Main(m *testing.M) {
	code := m.Run()
	stop()
	os.Exit(code)
}

// Available 报告是否有可用实例；不可用时返回原因。不会让测试失败或跳过。
func Available() (bool, string) {
	s := get()
	switch {
	case s.err != nil:
		return false, s.err.Error()
	case s.skip != "":
		return false, s.skip
	}
	return true, ""
}

// NewDatabase 新建一个空数据库并返回其连接串；测试结束时删除。
func NewDatabase(t testing.TB) string {
	t.Helper()
	s := get()
	if s.err != nil {
		t.Fatal(s.err)
	}
	if s.skip != "" {
		if os.Getenv(EnvRequire) == "1" {
			t.Fatal(s.skip)
		}
		t.Skip(s.skip)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "isp_t_" + hex.EncodeToString(b[:])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, s.adminURL)
	if err != nil {
		t.Fatalf("pgtest: 连接测试实例: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE template0 ENCODING 'UTF8'"); err != nil {
		t.Fatalf("pgtest: 建库: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, s.adminURL)
		if err != nil {
			t.Logf("pgtest: 删库时连接失败: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("pgtest: 删库 %s: %v", name, err)
		}
	})

	u, err := url.Parse(s.adminURL)
	if err != nil {
		t.Fatalf("pgtest: 解析连接串: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// Open 连接到 url 指向的数据库，测试结束时关闭连接池。
func Open(t testing.TB, dbURL string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// New 返回一个已执行全部内嵌迁移的新数据库连接池。
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := Open(t, NewDatabase(t))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("pgtest: 迁移: %v", err)
	}
	return pool
}
