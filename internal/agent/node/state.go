package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
)

// 数据目录中的文件。
const (
	usedFile  = "used.json"    // agent.FileStore：各订阅累计用量
	stateFile = "desired.json" // 最后一次已应用的期望状态
)

// stateFormat 是 desired.json 的格式版本。
const stateFormat = 1

// savedState 是落盘的“最后一次已应用的期望状态”（docs/04 §8 局限 6）：节点在控制面离线时重启，
// 先按它恢复接入并继续执行额度与期限，不等控制面重新下发。
//
// 只记录已成功应用的订阅条目（版本即上报中的 applied_version），失败的更高版本不落盘。
// 条目含接入密码与上游凭据，所以文件权限为 0600、数据目录为 0700；不得复制到日志或备份到别处。
type savedState struct {
	Format        int                            `json:"format"`
	Revision      uint64                         `json:"revision"`
	SavedAt       time.Time                      `json:"saved_at"`
	Subscriptions []agentproto.SubscriptionEntry `json:"subscriptions"`
}

func loadState(path string) (*savedState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &savedState{Format: stateFormat}, nil
	}
	if err != nil {
		return nil, err
	}
	var s savedState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Format != stateFormat {
		return nil, fmt.Errorf("%s: 不认识的格式版本 %d", path, s.Format)
	}
	// 收紧被外部放宽的权限。
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
	return &s, nil
}

// writeFileAtomic 以“写临时文件（0600）+ fsync + rename + 同步目录”的方式原子替换 path。
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".desired-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func saveState(path string, s *savedState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}
