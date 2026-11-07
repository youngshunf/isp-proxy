package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Store 持久化各订阅的累计已用量。
type Store interface {
	Load() (map[string]uint64, error)
	Save(map[string]uint64) error
}

// FileStore 以“写临时文件 + fsync + rename”的方式原子落盘。
type FileStore struct{ Path string }

func (f FileStore) Load() (map[string]uint64, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]uint64{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]uint64{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f FileStore) Save(m map[string]uint64) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".used-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
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
	return os.Rename(tmp.Name(), f.Path)
}
