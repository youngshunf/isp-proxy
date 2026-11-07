package mail

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Worker 从发件箱取出到期邮件并投递。可多进程同时运行（行锁 SKIP LOCKED）。
//
// 重试：第 n 次失败后，下一次尝试不早于失败时刻 + Backoff×2ⁿ⁻¹（记在 payload.next）；达到 MaxAttempts 次失败标记 failed。
// 发送失败不影响业务：用户可以“重新发送”拿到新的验证码（旧验证码随之失效）。
type Worker struct {
	Pool        *pgxpool.Pool
	Box         *secret.Box
	Sender      Sender
	From        string // 发件人，如 "示例代理 <no-reply@example.com>"
	Brand       Brand
	Interval    time.Duration // 轮询间隔，默认 2 秒
	Backoff     time.Duration // 退避基数，默认 10 秒
	MaxAttempts int           // 默认 8
	Logger      *slog.Logger
	Now         func() time.Time

	kick chan struct{}
}

func (w *Worker) defaults() {
	if w.Interval <= 0 {
		w.Interval = 2 * time.Second
	}
	if w.Backoff <= 0 {
		w.Backoff = 10 * time.Second
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = 8
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	if w.Now == nil {
		w.Now = time.Now
	}
}

// Kick 让 Run 立即检查一次（业务事务提交后调用，缩短验证码到达时间）。可在 Run 之前调用。
func (w *Worker) Kick() {
	if w.kick == nil {
		return
	}
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Init 准备 Kick 通道；Run 会自动调用。需要在 Run 启动前就能 Kick 时先调用 Init。
func (w *Worker) Init() {
	if w.kick == nil {
		w.kick = make(chan struct{}, 1)
	}
}

// Run 循环投递直到 ctx 结束。
func (w *Worker) Run(ctx context.Context) {
	w.defaults()
	w.Init()
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		if _, err := w.DeliverDue(ctx); err != nil && ctx.Err() == nil {
			w.Logger.Error("mail: 投递循环出错", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

// Result 是一次 DeliverDue 的统计。
type Result struct {
	Sent, Retried, Failed int
}

// DeliverDue 投递当前到期的全部邮件（每封一个事务），返回统计。
func (w *Worker) DeliverDue(ctx context.Context) (Result, error) {
	w.defaults()
	var res Result
	for ctx.Err() == nil {
		done, err := w.deliverOne(ctx, &res)
		if err != nil {
			return res, err
		}
		if done {
			break
		}
	}
	return res, ctx.Err()
}

func (w *Worker) deliverOne(ctx context.Context, res *Result) (empty bool, err error) {
	now := w.Now()
	err = store.InTx(ctx, w.Pool, func(tx pgx.Tx) error {
		var id, to, tmpl string
		var payload []byte
		var attempts int
		err := tx.QueryRow(ctx, `
			SELECT id, to_email, template, payload, attempts FROM mail_outbox
			 WHERE status = 'queued' AND coalesce((payload->>'next')::timestamptz, created_at) <= $1
			 ORDER BY created_at
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED`, now).Scan(&id, &to, &tmpl, &payload, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			empty = true
			return nil
		}
		if err != nil {
			return err
		}
		sendErr := w.send(ctx, id, to, tmpl, payload)
		if sendErr == nil {
			res.Sent++
			_, err = tx.Exec(ctx, `UPDATE mail_outbox SET status = 'sent', sent_at = $2, attempts = attempts + 1,
				last_error = NULL, payload = '{}' WHERE id = $1`, id, now)
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := sendErr.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if attempts+1 >= w.MaxAttempts {
			res.Failed++
			w.Logger.Error("mail: 多次投递失败，放弃", "id", id, "template", tmpl, "attempts", attempts+1, "err", sendErr)
			_, err = tx.Exec(ctx, `UPDATE mail_outbox SET status = 'failed', attempts = attempts + 1, last_error = $2,
				payload = '{}' WHERE id = $1`, id, msg)
			return err
		}
		res.Retried++
		w.Logger.Warn("mail: 投递失败，稍后重试", "id", id, "template", tmpl, "attempts", attempts+1, "err", sendErr)
		next := now.Add(w.Backoff << min(attempts, 16))
		_, err = tx.Exec(ctx, `UPDATE mail_outbox SET attempts = attempts + 1, last_error = $2,
			payload = jsonb_set(payload, '{next}', to_jsonb($3::timestamptz)) WHERE id = $1`, id, msg, next)
		return err
	})
	return empty, err
}

func (w *Worker) send(ctx context.Context, id, to, tmpl string, payload []byte) error {
	data, err := openData(w.Box, id, payload)
	if err != nil {
		return err
	}
	subject, body, err := Render(w.Brand, tmpl, data)
	if err != nil {
		return err
	}
	return w.Sender.Send(ctx, &Rendered{ID: id, From: w.From, To: to, Subject: subject, Body: body})
}
