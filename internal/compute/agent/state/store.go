// Package state 是 Compute Agent 的本地状态（ADR-0015"Agent 与控制面的通信"）：
// 每个对象一个文件，informer 只负责更新它，本地 reconcile 只读它，控制面不可用时照样能按它收敛。
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Record 是一个对象在本节点的期望与已应用的事实。
type Record struct {
	Kind string `json:"kind"`
	// Key 是 name（集群级对象）或 namespace/name。
	Key             string `json:"key"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	// Object 是从 API 收到的完整对象。
	Object json.RawMessage `json:"object"`
	// Deleted 表示 API 确认对象已不再属于本节点；清理完成前保留，Agent 崩溃后能继续清理。
	Deleted bool `json:"deleted,omitempty"`

	// AppliedInputHash 是最近一次下发时 Agent 生成的 XML 的 hash；与本轮不同说明 spec 或解析结果变了。
	AppliedInputHash string `json:"appliedInputHash,omitempty"`
	// AppliedHash 是下发后读回的 libvirt 规范化 XML 的 hash；与现状不同说明有人在带外修改过。
	AppliedHash string `json:"appliedHash,omitempty"`
	// PoweredOff 记录观察到的有意停止（客户机关机或 destroy），宿主机重启后不再自动拉起。
	PoweredOff bool `json:"poweredOff,omitempty"`
}

type Store struct {
	dir     string
	mu      sync.Mutex
	records map[string]Record
	changed chan struct{}
}

// Open 加载 dir 中已有的记录；目录不存在时创建。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, records: map[string]Record{}, changed: make(chan struct{}, 1)}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		s.records[id(r.Kind, r.Key)] = r
	}
	return s, nil
}

// Changed 在记录变化后收到通知（合并多次变化）。
func (s *Store) Changed() <-chan struct{} { return s.changed }

// Put 写入从 API 收到的对象。只接受比现有记录更新的 resourceVersion，返回是否写入。
// 已应用的事实保留不变。
func (s *Store) Put(kind, key, uid, resourceVersion string, object json.RawMessage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id(kind, key)]
	if ok && r.UID == uid && !r.Deleted && !newer(resourceVersion, r.ResourceVersion) {
		return false, nil
	}
	if !ok || r.UID != uid {
		// 新对象，或同名对象被删除后重建：已应用的事实属于旧对象，不能沿用。
		r = Record{Kind: kind, Key: key, UID: uid}
	}
	r.ResourceVersion, r.Object, r.Deleted = resourceVersion, object, false
	return true, s.write(r)
}

// MarkDeleted 标记对象已不属于本节点，等待 reconcile 清理后 Forget。
func (s *Store) MarkDeleted(kind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id(kind, key)]
	if !ok || r.Deleted {
		return nil
	}
	r.Deleted = true
	return s.write(r)
}

// Update 修改已应用的事实；记录不存在时什么也不做。
func (s *Store) Update(kind, key string, update func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id(kind, key)]
	if !ok {
		return nil
	}
	before := r
	update(&r)
	if r.AppliedInputHash == before.AppliedInputHash && r.AppliedHash == before.AppliedHash && r.PoweredOff == before.PoweredOff {
		return nil
	}
	return s.write(r)
}

// Forget 在清理完成后删除记录文件。
func (s *Store) Forget(kind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(kind, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(s.records, id(kind, key))
	s.notify()
	return nil
}

// List 按 kind 返回记录的副本，按 Key 排序。
func (s *Store) List(kind string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.records {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// write 先写临时文件并 fsync，再 rename，保证文件要么是旧内容、要么是新内容。
func (s *Store) write(r Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(r.Kind, r.Key)); err != nil {
		return err
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	s.records[id(r.Kind, r.Key)] = r
	s.notify()
	return nil
}

func (s *Store) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Kubernetes 的名字里不会出现 "_"，用它替换 "/" 得到的文件名不会冲突。
func (s *Store) path(kind, key string) string {
	return filepath.Join(s.dir, kind+"_"+strings.ReplaceAll(key, "/", "_")+".json")
}

func id(kind, key string) string { return kind + "/" + key }

// newer 判断 a 是否比 b 新。
// ponytail: resourceVersion 按约定是不透明的，这里依赖 etcd 后端的单调整数；不能解析时视为更新。
// etcd 从旧备份恢复后，旧版本会被拒绝，直到对象再次变更。
func newer(a, b string) bool {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		return a != b
	}
	return x > y
}
