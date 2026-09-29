package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store 是带互斥、JSON 持久化的简单状态库，保证重启后状态不丢。
type Store[T any] struct {
	mu   sync.Mutex
	path string
	data T
}

func New[T any](path string, empty T) (*Store[T], error) {
	s := &Store[T]{path: path, data: empty}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store[T]) View(fn func(*T)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
}

// Update 修改状态并原子落盘（先写临时文件再 rename）。
func (s *Store[T]) Update(fn func(*T)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
