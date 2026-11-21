// Package station 保存采集站状态：每个区的历史快照（旧区一直可查询）
// 与全部传送记录（成功与失败都保存，失败结果可单独检查）。
package station

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"zonestation/internal/collector"
	"zonestation/internal/zone"
)

// Snapshot 是区内容的一份历史快照。
type Snapshot struct {
	Zone       *zone.Zone
	TakenAt    time.Time
	TransferID int64
}

// Store 是采集站的状态存储（内存实现，并发安全）。
type Store struct {
	mu         sync.RWMutex
	transferMu sync.Mutex // 串行化传送，避免同一区并发切换
	snaps      map[string][]*Snapshot
	results    map[int64]*collector.Result
	byZone     map[string][]int64
	nextID     int64
}

// New 创建空存储。
func New() *Store {
	return &Store{
		snaps:   map[string][]*Snapshot{},
		results: map[int64]*collector.Result{},
		byZone:  map[string][]int64{},
	}
}

// Transfer 执行一次传送并记录结果；成功时保存新快照。
// 失败不会改动任何已有快照，旧区保持可查询。
func (s *Store) Transfer(svc *collector.Service, zoneName string, mode collector.Mode) *collector.Result {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()

	s.mu.Lock()
	s.nextID++
	id := s.nextID
	local := s.currentLocked(zone.Canonical(zoneName))
	s.mu.Unlock()

	after, res := svc.Execute(id, zoneName, mode, local)

	s.mu.Lock()
	defer s.mu.Unlock()
	if res.Success && !res.UpToDate && after != nil {
		s.snaps[res.Zone] = append(s.snaps[res.Zone], &Snapshot{
			Zone:       after,
			TakenAt:    res.FinishedAt,
			TransferID: res.ID,
		})
	}
	s.results[id] = res
	s.byZone[res.Zone] = append(s.byZone[res.Zone], id)
	return res
}

// currentLocked 返回当前快照（调用时需持锁）。
func (s *Store) currentLocked(origin string) *zone.Zone {
	list := s.snaps[origin]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1].Zone
}

// Current 返回区的当前快照；没有副本时返回 nil。
func (s *Store) Current(origin string) *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.snaps[zone.Canonical(origin)]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

// Snapshots 列出区的全部快照（旧的在前），供“旧区一直可查询”。
func (s *Store) Snapshots(origin string) []*Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.snaps[zone.Canonical(origin)]
	out := make([]*Snapshot, len(list))
	copy(out, list)
	return out
}

// SnapshotAt 按序列号取某个历史快照。
func (s *Store) SnapshotAt(origin string, serial uint32) *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, snap := range s.snaps[zone.Canonical(origin)] {
		if snap.Zone.Serial == serial {
			return snap
		}
	}
	return nil
}

// ClearZone 删除某区的全部本地快照（传送记录保留）。
func (s *Store) ClearZone(origin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snaps, zone.Canonical(origin))
}

// Result 取一次传送记录。
func (s *Store) Result(id int64) *collector.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.results[id]
}

// Results 列出传送记录（新的在前）；zoneFilter 为空表示全部区，
// failedOnly 为真时只列失败结果（失败结果单独检查）。
func (s *Store) Results(zoneFilter string, failedOnly bool) []*collector.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*collector.Result
	for _, r := range s.results {
		if zoneFilter != "" && r.Zone != zone.Canonical(zoneFilter) {
			continue
		}
		if failedOnly && r.Success {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// ZoneSummary 是采集站视角下一个区的概览。
type ZoneSummary struct {
	Name         string `json:"name"`
	HasCopy      bool   `json:"hasCopy"`
	Serial       uint32 `json:"serial"`
	SnapshotNum  int    `json:"snapshotNum"`
	TransferNum  int    `json:"transferNum"`
	FailedNum    int    `json:"failedNum"`
	LastTransfer int64  `json:"lastTransfer"`
}

// Summaries 返回采集站所有已知区的概览。
func (s *Store) Summaries() []ZoneSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := map[string]bool{}
	for name := range s.snaps {
		names[name] = true
	}
	for name := range s.byZone {
		names[name] = true
	}
	out := make([]ZoneSummary, 0, len(names))
	for name := range names {
		sum := ZoneSummary{Name: name}
		if list := s.snaps[name]; len(list) > 0 {
			sum.HasCopy = true
			sum.Serial = list[len(list)-1].Zone.Serial
			sum.SnapshotNum = len(list)
		}
		for _, id := range s.byZone[name] {
			r := s.results[id]
			if r == nil {
				continue
			}
			sum.TransferNum++
			if !r.Success {
				sum.FailedNum++
			}
			if id > sum.LastTransfer {
				sum.LastTransfer = id
			}
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// String 便于日志输出。
func (s *Store) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("station.Store{zones=%d transfers=%d}", len(s.snaps), len(s.results))
}
