package account

import (
	"context"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
)

// 供管理端（internal/control/admin）在其事务内调用的账号操作。管理员不能查看或设置用户密码（R-15.5）。

// CreateByAdmin 代建用户：只建邮箱、不设密码，并发送“设置登录密码”验证码；用户用 confirmPasswordReset
// 设置密码并完成验证（R-15.1）。邮箱已存在返回 Conflict。
func (s *Service) CreateByAdmin(ctx context.Context, tx pgx.Tx, rawEmail, adminID string) (string, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO users (id, email, created_by_admin, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO NOTHING RETURNING id`, store.NewID("usr"), email, adminID, s.now()).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", httpx.Conflict("该邮箱已有账号")
	}
	if err != nil {
		return "", err
	}
	if err := s.TakeMailQuota(ctx, tx, email); err != nil {
		return "", err
	}
	u := &userRow{ID: id, Email: email}
	if err := s.issueReset(ctx, tx, u); err != nil {
		return "", err
	}
	return id, nil
}

// SendResetByAdmin 为用户发送重设（或首次设置）密码的验证码。用户不存在返回 NotFound；受发信限流约束。
func (s *Service) SendResetByAdmin(ctx context.Context, tx pgx.Tx, userID string) error {
	u, err := userByID(ctx, tx, userID, true)
	if err != nil {
		return err
	}
	if u == nil {
		return httpx.NotFound("找不到该用户")
	}
	if err := s.TakeMailQuota(ctx, tx, u.Email); err != nil {
		return err
	}
	return s.issueReset(ctx, tx, u)
}

// KickMail 唤醒发件箱投递（事务提交之后调用）。
func (s *Service) KickMail() { s.kickMail() }
