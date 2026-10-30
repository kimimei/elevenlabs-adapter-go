package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store 是任务表在磁盘上的表示：一个任务两个文件。
//
//	<dir>/<id>.json   元数据
//	<dir>/<id>.audio  音频字节（仅成功任务）
//
// 为什么不是一个大的 jobs.json：整表重写要读-改-写，两个 worker 同时落盘就互相
// 覆盖；每任务一个文件是「谁的写谁负责」，天然没有跨任务竞争，进程崩在写一半
// 也只坏这一个任务的文件（还有 .tmp + rename 兜着）。
type Store struct {
	dir string
}

// NewStore 准备目录并做一次**可写性探测**。
//
// 探测是刻意的：任务表落盘是这个模块存在的理由，目录不可写却照常启动，
// 等于对外承诺「重启不丢」然后在重启时把任务全丢了。宁可启动失败。
func NewStore(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("任务目录为空（ADAPTER_JOBS_DIR）")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("任务目录创建失败: %w", err)
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf("任务目录不可写（%s）: %w", dir, err)
	}
	_ = os.Remove(probe)
	return &Store{dir: dir}, nil
}

// Dir 返回任务目录。
func (s *Store) Dir() string { return s.dir }

func (s *Store) metaPath(id string) string  { return filepath.Join(s.dir, id+".json") }
func (s *Store) audioPath(id string) string { return filepath.Join(s.dir, id+".audio") }

// Save 写元数据。原子替换：先写 .tmp 再 rename，
// 断电/被 OOM 杀掉时要么是旧内容要么是新内容，不会是半个 JSON。
func (s *Store) Save(j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("任务序列化失败: %w", err)
	}
	return writeAtomic(s.metaPath(j.ID), data)
}

// SaveAudio 写音频字节。
func (s *Store) SaveAudio(id string, data []byte) error {
	return writeAtomic(s.audioPath(id), data)
}

// LoadAudio 读音频字节。
func (s *Store) LoadAudio(id string) ([]byte, error) { return os.ReadFile(s.audioPath(id)) }

// Delete 删除任务的两个文件。文件不存在不算错误（清理是幂等的）。
func (s *Store) Delete(id string) error {
	var firstErr error
	for _, p := range []string{s.metaPath(id), s.audioPath(id)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Load 读回全部任务。
//
// 坏文件直接跳过：一条脏数据不该让适配器起不来 —— 启动失败意味着
// 所有客户一起不可用，代价远大于丢一条任务记录。
func (s *Store) Load() ([]*Job, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Job, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue // .tmp 残留与音频文件都跳过
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			continue
		}
		if j.ID == "" {
			j.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		out = append(out, &j)
	}
	return out, nil
}

// HasAudio 判断音频文件是否存在（恢复时校验用）。
func (s *Store) HasAudio(id string) bool {
	st, err := os.Stat(s.audioPath(id))
	return err == nil && st.Size() > 0
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Sync 后再 rename：否则断电后可能出现「文件已改名但内容为空」
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
