package store_test

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/store/pgtest"
	"github.com/go-gost/gost/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 0001 应建出的表（docs/04 §7.4 + M1 补充）。
var wantTables = []string{
	"access_versions", "admin_recovery_codes", "admins", "alerts", "audit_logs", "email_tokens", "exit_bindings",
	"exit_requests", "idempotency_keys", "ledger_entries", "mail_outbox", "nodes", "order_lines", "orders",
	"payments", "plan_versions", "plans", "ports", "probes", "rate_events", "refunds", "rules", "schema_migrations",
	"sessions", "subscription_events", "subscriptions", "tasks", "traffic_pack_versions", "traffic_packs", "transfers",
	"upstream_resources", "usage_reports", "usage_samples", "users",
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestMigrateEmptyThenNoop(t *testing.T) {
	c := ctx(t)
	pool := pgtest.Open(t, pgtest.NewDatabase(t))

	all, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[0].Version != 1 {
		t.Fatalf("内嵌迁移应从 0001 开始，得到 %+v", all)
	}

	applied, err := store.Migrate(c, pool)
	if err != nil {
		t.Fatalf("首次迁移: %v", err)
	}
	if len(applied) != len(all) {
		t.Fatalf("首次迁移应执行全部 %d 个文件，实际 %d", len(all), len(applied))
	}

	rows, err := pool.Query(c, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
	}
	for _, w := range wantTables {
		if !have[w] {
			t.Errorf("缺少表 %s", w)
		}
	}

	again, err := store.Migrate(c, pool)
	if err != nil {
		t.Fatalf("重复迁移: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("重复迁移应为空操作，却执行了 %d 个", len(again))
	}
	vs, err := store.AppliedVersions(c, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != len(all) {
		t.Fatalf("schema_migrations 应记录 %d 个版本，实际 %v", len(all), vs)
	}
}

func TestMigrateConcurrentStartup(t *testing.T) {
	c := ctx(t)
	url := pgtest.NewDatabase(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	total := make([]int, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pool, err := store.Open(c, url)
			if err != nil {
				errs[i] = err
				return
			}
			defer pool.Close()
			ms, err := store.Migrate(c, pool)
			errs[i], total[i] = err, len(ms)
		}(i)
	}
	wg.Wait()
	sum := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发迁移 %d: %v", i, err)
		}
		sum += total[i]
	}
	all, _ := store.LoadMigrations(migrations.FS)
	if sum != len(all) {
		t.Fatalf("并发启动时每个迁移只应执行一次：共执行 %d，应为 %d", sum, len(all))
	}
}

func TestMigrateRejectsEditedFile(t *testing.T) {
	c := ctx(t)
	pool := pgtest.Open(t, pgtest.NewDatabase(t))
	v1 := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (x int);")}}
	if _, err := store.MigrateFS(c, pool, v1); err != nil {
		t.Fatal(err)
	}
	edited := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (x bigint);")}}
	if _, err := store.MigrateFS(c, pool, edited); err == nil {
		t.Fatal("已执行的迁移被修改后应报错")
	}
	// 失败的迁移整体回滚，不留下半截 schema。
	broken := fstest.MapFS{
		"0001_a.sql": v1["0001_a.sql"],
		"0002_b.sql": {Data: []byte("CREATE TABLE b (x int); SELECT no_such_function();")},
	}
	if _, err := store.MigrateFS(c, pool, broken); err == nil {
		t.Fatal("出错的迁移应返回错误")
	}
	var n int
	if err := pool.QueryRow(c, "SELECT count(*) FROM pg_tables WHERE tablename = 'b'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("失败迁移应回滚: n=%d err=%v", n, err)
	}
	if _, err := store.MigrateFS(c, pool, fstest.MapFS{}); err == nil {
		t.Fatal("库里有本程序不认识的版本时应报错")
	}
}

func TestNewIDFormat(t *testing.T) {
	c := ctx(t)
	pool := pgtest.New(t)
	re := regexp.MustCompile(`^sub_[0-9a-hjkmnp-tv-z]{20}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := store.NewID("sub")
		if !re.MatchString(id) || seen[id] {
			t.Fatalf("NewID 格式或唯一性错误: %q", id)
		}
		seen[id] = true
	}
	var ok bool
	var dbID string
	if err := pool.QueryRow(c, "SELECT isp_id_ok($1, 'sub'), isp_new_id('sub')", store.NewID("sub")).Scan(&ok, &dbID); err != nil {
		t.Fatal(err)
	}
	if !ok || !re.MatchString(dbID) {
		t.Fatalf("Go 与 SQL 的 ID 格式应一致: ok=%v db=%q", ok, dbID)
	}
	if _, err := pool.Exec(c, "INSERT INTO users (id, email) VALUES ('usr_1', 'x@example.com')"); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("可枚举的短 ID 应被拒绝，得到 %v", err)
	}
}

// ---- 不变量 ----

type fixture struct {
	pool   *pgxpool.Pool
	user   string
	user2  string
	plan   string
	planV  string
	sub    string // user 的订阅，已入账 50 GB + 30 天并起算
	sub2   string // user 的第二个订阅
	subOf2 string // user2 的订阅
}

func setup(t *testing.T) *fixture {
	t.Helper()
	c := ctx(t)
	f := &fixture{pool: pgtest.New(t)}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(c, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	f.user = q("INSERT INTO users (email, email_verified_at) VALUES ('a@example.com', now()) RETURNING id")
	f.user2 = q("INSERT INTO users (email, email_verified_at) VALUES ('b@example.com', now()) RETURNING id")
	f.plan = q("INSERT INTO plans (code, status) VALUES ('us-lax', 'on_sale') RETURNING id")
	f.planV = q(`INSERT INTO plan_versions (plan_id, version, name, country_code, country_name, city, price_cents,
	                 period_days, included_bytes, concurrency, bandwidth_mbps)
	             VALUES ($1, 1, '美国 · 洛杉矶 包月', 'us', '美国', '洛杉矶', 16800, 30, 50000000000, 50, 20) RETURNING id`, f.plan)
	newSub := func(user, name string) string {
		return q(`INSERT INTO subscriptions (user_id, plan_id, plan_version_id, rules_version, name)
		          VALUES ($1, $2, $3, 'R-2026-11-08', $4) RETURNING id`, user, f.plan, f.planV, name)
	}
	f.sub, f.sub2, f.subOf2 = newSub(f.user, "一号"), newSub(f.user, "二号"), newSub(f.user2, "别人的")

	// 首购：模拟确认收款后的入账，再在就绪时起算。
	order := q(`INSERT INTO orders (user_id, kind, subscription_id, plan_id, plan_version_id, months, amount_cents,
	                rules_version, rules_snapshot, hold_until)
	            VALUES ($1, 'new', $2, $3, $4, 1, 16800, 'R-2026-11-08', '{}', now() + interval '24 hours') RETURNING id`,
		f.user, f.sub, f.plan, f.planV)
	err := store.InTx(c, f.pool, func(tx pgx.Tx) error {
		for _, e := range []struct {
			kind string
			amt  int64
		}{{"traffic", 50_000_000_000}, {"time", 30 * 86400}} {
			if _, err := tx.Exec(c, `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, order_id)
			                          VALUES ($1, $2, $3, 'order', $4, $4)`, f.sub, e.kind, e.amt, order); err != nil {
				return err
			}
		}
		_, err := tx.Exec(c, `UPDATE subscriptions SET lifecycle = 'active', first_ready_at = now(), period_started_at = now(),
		                          expires_at = now() + make_interval(secs => entitled_seconds_total - seconds_before_period)
		                       WHERE id = $1`, f.sub)
		return err
	})
	if err != nil {
		t.Fatalf("入账与起算: %v", err)
	}
	return f
}

func (f *fixture) totals(t *testing.T, sub string) (entitled, used, seconds int64) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		"SELECT entitled_bytes_total, used_bytes_total, entitled_seconds_total FROM subscriptions WHERE id = $1", sub,
	).Scan(&entitled, &used, &seconds); err != nil {
		t.Fatal(err)
	}
	return
}

func TestLedgerSourceKeyIsIdempotent(t *testing.T) {
	c := ctx(t)
	f := setup(t)
	before, _, _ := f.totals(t, f.sub)

	ins := `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, admin_id, reason)
	        VALUES ($1, 'traffic', 1000, 'gift', 'adj_dup', $2, '补偿')`
	admin := mustAdmin(t, f)
	if _, err := f.pool.Exec(c, ins, f.sub, admin); err != nil {
		t.Fatal(err)
	}
	_, err := f.pool.Exec(c, ins, f.sub, admin)
	if !store.IsUniqueViolation(err, "ledger_entries_source_key") {
		t.Fatalf("同一业务来源键第二次入账应被唯一键拒绝，得到 %v", err)
	}
	// ON CONFLICT DO NOTHING 跳过的重复条目不得改变权益合计（触发器为 AFTER INSERT）。
	tag, err := f.pool.Exec(c, ins+" ON CONFLICT ON CONSTRAINT ledger_entries_source_key DO NOTHING", f.sub, admin)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("ON CONFLICT 应静默跳过: rows=%d err=%v", tag.RowsAffected(), err)
	}
	after, _, _ := f.totals(t, f.sub)
	if after != before+1000 {
		t.Fatalf("权益合计应只增加一次 1000：before=%d after=%d", before, after)
	}
	// 账本只追加。
	if _, err := f.pool.Exec(c, "UPDATE ledger_entries SET amount = 1 WHERE source_id = 'adj_dup'"); store.PgCode(err) != store.CodeRestrictViolation {
		t.Fatalf("账本条目不能修改，得到 %v", err)
	}
	if _, err := f.pool.Exec(c, "DELETE FROM ledger_entries"); store.PgCode(err) != store.CodeRestrictViolation {
		t.Fatalf("账本条目不能删除，得到 %v", err)
	}
}

func TestEntitlementAndUsageInvariants(t *testing.T) {
	c := ctx(t)
	f := setup(t)
	admin := mustAdmin(t, f)

	// 权益合计不能直接写。
	if _, err := f.pool.Exec(c, "UPDATE subscriptions SET entitled_bytes_total = 1 WHERE id = $1", f.sub); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("直接写 entitled_bytes_total 应被拒绝，得到 %v", err)
	}
	// 扣回超过余额 → 合计为负 → 整体失败，不产生负权益。
	_, err := f.pool.Exec(c, `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, admin_id, reason)
	                          VALUES ($1, 'traffic', -60000000000, 'adjust', 'adj_neg', $2, '误操作')`, f.sub, admin)
	if store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("扣成负权益应被拒绝，得到 %v", err)
	}
	if e, _, _ := f.totals(t, f.sub); e != 50_000_000_000 {
		t.Fatalf("失败的扣回不应改变合计，得到 %d", e)
	}
	// used 不能为负、不能回退；GREATEST 合并是幂等的。
	if _, err := f.pool.Exec(c, "UPDATE subscriptions SET used_bytes_total = -1 WHERE id = $1", f.sub); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("负的 used 应被拒绝，得到 %v", err)
	}
	merge := "UPDATE subscriptions SET used_bytes_total = GREATEST(used_bytes_total, $2) WHERE id = $1"
	for _, v := range []int64{500, 300, 500, 900, 100} { // 乱序、重放
		if _, err := f.pool.Exec(c, merge, f.sub, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, u, _ := f.totals(t, f.sub); u != 900 {
		t.Fatalf("GREATEST 合并后 used 应为 900，得到 %d", u)
	}
	if _, err := f.pool.Exec(c, "UPDATE subscriptions SET used_bytes_total = 10 WHERE id = $1", f.sub); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("used 回退应被拒绝，得到 %v", err)
	}

	// 有效期内续时长：账本入账即顺延到期时间。
	var before, after time.Time
	_ = f.pool.QueryRow(c, "SELECT expires_at FROM subscriptions WHERE id = $1", f.sub).Scan(&before)
	if _, err := f.pool.Exec(c, `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, admin_id, reason)
	                             VALUES ($1, 'time', 86400, 'compensation', 'adj_time', $2, '补时')`, f.sub, admin); err != nil {
		t.Fatal(err)
	}
	_ = f.pool.QueryRow(c, "SELECT expires_at FROM subscriptions WHERE id = $1", f.sub).Scan(&after)
	if after.Sub(before) != 24*time.Hour {
		t.Fatalf("补时 1 天应顺延 24h，得到 %v", after.Sub(before))
	}
	// 到期时间与时长账本不一致 → 提交时被拒绝。
	if _, err := f.pool.Exec(c, "UPDATE subscriptions SET expires_at = expires_at + interval '1 hour' WHERE id = $1", f.sub); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("手改到期时间应在提交时被拒绝，得到 %v", err)
	}
}

func TestOneInProgressTaskPerSubscription(t *testing.T) {
	c := ctx(t)
	f := setup(t)
	ins := `INSERT INTO tasks (subscription_id, kind, requested_by_kind, requested_by_user) VALUES ($1, $2, 'user', $3) RETURNING id`
	var first string
	if err := f.pool.QueryRow(c, ins, f.sub, "access_change", f.user).Scan(&first); err != nil {
		t.Fatal(err)
	}
	var x string
	err := f.pool.QueryRow(c, ins, f.sub, "password_reset", f.user).Scan(&x)
	if !store.IsUniqueViolation(err, "tasks_one_in_progress") {
		t.Fatalf("同一订阅第二个进行中任务应被拒绝，得到 %v", err)
	}
	// 其他订阅不受影响；前一个任务结束后可以再建。
	if err := f.pool.QueryRow(c, ins, f.sub2, "access_change", f.user).Scan(&x); err != nil {
		t.Fatalf("其他订阅应可建任务: %v", err)
	}
	if _, err := f.pool.Exec(c, "UPDATE tasks SET state = 'succeeded', finished_at = now() WHERE id = $1", first); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(c, ins, f.sub, "password_reset", f.user).Scan(&x); err != nil {
		t.Fatalf("前一任务完成后应可再建: %v", err)
	}
}

func TestOneOpenExitRequestPerSubscription(t *testing.T) {
	c := ctx(t)
	f := setup(t)
	ins := `INSERT INTO exit_requests (subscription_id, user_id, reason, charge, fee_status) VALUES ($1, $2, 'quality', 'free', 'not_applicable') RETURNING id`
	var first, x string
	if err := f.pool.QueryRow(c, ins, f.sub, f.user).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(c, ins, f.sub, f.user).Scan(&x); !store.IsUniqueViolation(err, "exit_requests_one_open") {
		t.Fatalf("同一订阅第二个未完成申请应被拒绝，得到 %v", err)
	}
	// 申请人必须是订阅所属用户（复合外键）。
	if err := f.pool.QueryRow(c, ins, f.sub2, f.user2).Scan(&x); store.PgCode(err) != store.CodeForeignKeyViolation {
		t.Fatalf("为别人的订阅建申请应被拒绝，得到 %v", err)
	}
	// D-19：收费未确认前不能进入处理。
	var manual string
	if err := f.pool.QueryRow(c, `INSERT INTO exit_requests (subscription_id, user_id, reason, charge, fee_status)
	                               VALUES ($1, $2, 'other', 'manual', 'awaiting_decision') RETURNING id`, f.sub2, f.user).Scan(&manual); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(c, "UPDATE exit_requests SET status = 'processing' WHERE id = $1", manual); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("待确认收费的申请不能进入处理，得到 %v", err)
	}
	if _, err := f.pool.Exec(c, "UPDATE exit_requests SET status = 'cancelled', finished_at = now() WHERE id = $1", first); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(c, ins, f.sub, f.user).Scan(&x); err != nil {
		t.Fatalf("旧申请结束后应可再申请: %v", err)
	}
}

func TestTransferMustBeSameUserAndPaired(t *testing.T) {
	c := ctx(t)
	f := setup(t)
	admin := mustAdmin(t, f)
	// 跨用户划转被复合外键拒绝。
	_, err := f.pool.Exec(c, `INSERT INTO transfers (user_id, from_subscription_id, to_subscription_id, bytes, reason, admin_id)
	                          VALUES ($1, $2, $3, 1, 'x', $4)`, f.user, f.sub, f.subOf2, admin)
	if store.PgCode(err) != store.CodeForeignKeyViolation {
		t.Fatalf("跨用户划转应被拒绝，得到 %v", err)
	}
	// 只有一条出账、没有入账 → 提交时被拒绝。
	err = store.InTx(c, f.pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(c, `INSERT INTO transfers (user_id, from_subscription_id, to_subscription_id, bytes, reason, admin_id)
		                          VALUES ($1, $2, $3, 1000, '划转', $4) RETURNING id`, f.user, f.sub, f.sub2, admin).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(c, `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, transfer_id)
		                      VALUES ($1, 'traffic', -1000, 'transfer_out', $2, $2)`, f.sub, id)
		return err
	})
	if store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("不成对的划转应在提交时被拒绝，得到 %v", err)
	}
	// 成对划转成功，两边合计守恒。
	err = store.InTx(c, f.pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(c, `INSERT INTO transfers (user_id, from_subscription_id, to_subscription_id, bytes, reason, admin_id)
		                          VALUES ($1, $2, $3, 1000, '划转', $4) RETURNING id`, f.user, f.sub, f.sub2, admin).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, transfer_id)
		                         VALUES ($1, 'traffic', -1000, 'transfer_out', $3, $3), ($2, 'traffic', 1000, 'transfer_in', $3, $3)`,
			f.sub, f.sub2, id); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("成对划转: %v", err)
	}
	a, _, _ := f.totals(t, f.sub)
	b, _, _ := f.totals(t, f.sub2)
	if a != 50_000_000_000-1000 || b != 1000 {
		t.Fatalf("划转后合计错误: from=%d to=%d", a, b)
	}
}

func mustAdmin(t *testing.T, f *fixture) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		"INSERT INTO admins (email, display_name, password_hash) VALUES ('ops@example.com', '运营', 'x') ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id",
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
