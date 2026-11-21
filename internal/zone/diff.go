package zone

import (
	"sort"

	"github.com/miekg/dns"
)

// TTLChange 表示同一条记录（同 RDATA）的 TTL 发生了变化。
// 这是记录集内部的修改，而不是记录集的新增或删除。
type TTLChange struct {
	RData  string `json:"rdata"`
	OldTTL uint32 `json:"oldTtl"`
	NewTTL uint32 `json:"newTtl"`
}

// RRsetDiff 描述一个“两边都存在但内容不同”的记录集的变化。
type RRsetDiff struct {
	Key        string      `json:"key"`
	Name       string      `json:"name"`
	Type       uint16      `json:"-"`
	Class      uint16      `json:"-"`
	Added      []*Record   `json:"-"`
	Removed    []*Record   `json:"-"`
	TTLChanges []TTLChange `json:"ttlChanges"`
}

// RRsetView 是一个完整记录集的视图（用于整体新增/删除的展示）。
type RRsetView struct {
	Key     string    `json:"key"`
	Name    string    `json:"name"`
	Type    uint16    `json:"-"`
	Class   uint16    `json:"-"`
	Records []*Record `json:"-"`
}

// Diff 是传送前后两个区快照的对照结果，按记录集（名称，类别，类型）组织。
type Diff struct {
	Added     []RRsetView `json:"-"` // 只在新区出现的记录集
	Removed   []RRsetView `json:"-"` // 只在旧区出现的记录集
	Modified  []RRsetDiff `json:"-"` // 两边都有但内容有变化的记录集
	Unchanged int         `json:"unchanged"`
}

// Empty 报告对照结果是否完全没有变化。
func (d *Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Modified) == 0
}

// DiffZones 对照旧区与新区。
//
// 对照以记录集（名称，类别，类型）为单位：单条记录的 TTL 改动
// 在旧新区中都映射到同一个记录集键，因此只会体现为该记录集内部
// 的一条 TTLChange，而绝不会被误判为“删除整个记录集再新增”。
func DiffZones(oldZ, newZ *Zone) *Diff {
	d := &Diff{}
	oldSets := oldZ.RRsets()
	newSets := newZ.RRsets()

	keys := map[string]bool{}
	for k := range oldSets {
		keys[k] = true
	}
	for k := range newSets {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, k := range sorted {
		o, inOld := oldSets[k]
		n, inNew := newSets[k]
		switch {
		case !inOld:
			d.Added = append(d.Added, viewOf(k, n))
		case !inNew:
			d.Removed = append(d.Removed, viewOf(k, o))
		default:
			if rd, same := diffRRset(o, n); same {
				d.Unchanged++
			} else {
				d.Modified = append(d.Modified, rd)
			}
		}
	}
	return d
}

func viewOf(key string, recs []*Record) RRsetView {
	v := RRsetView{Key: key, Records: recs}
	if len(recs) > 0 {
		v.Name = recs[0].Name
		v.Type = recs[0].Type
		v.Class = recs[0].Class
	}
	return v
}

// diffRRset 对照同一个记录集的两个版本。
// 先按整条记录（含 TTL）对齐；剩下的一删一增若 RDATA 相同，
// 则配对为 TTL 改动而不是两条独立变化。
func diffRRset(oldRecs, newRecs []*Record) (RRsetDiff, bool) {
	rd := RRsetDiff{Key: oldRecs[0].SetKey(), Name: oldRecs[0].Name, Type: oldRecs[0].Type, Class: oldRecs[0].Class}

	oldByID := map[string]*Record{}
	for _, r := range oldRecs {
		oldByID[r.Identity()] = r
	}
	newByID := map[string]*Record{}
	for _, r := range newRecs {
		newByID[r.Identity()] = r
	}

	var removed, added []*Record
	for id, r := range oldByID {
		if _, ok := newByID[id]; !ok {
			removed = append(removed, r)
		}
	}
	for id, r := range newByID {
		if _, ok := oldByID[id]; !ok {
			added = append(added, r)
		}
	}

	// TTL 改动配对：删除侧与新增侧 RDATA 相同（仅 TTL 不同）的记录两两配对。
	addedByData := map[string][]*Record{}
	for _, r := range added {
		k := r.DataIdentity()
		addedByData[k] = append(addedByData[k], r)
	}
	var stillRemoved []*Record
	for _, r := range removed {
		k := r.DataIdentity()
		if cands := addedByData[k]; len(cands) > 0 {
			nr := cands[0]
			addedByData[k] = cands[1:]
			rd.TTLChanges = append(rd.TTLChanges, TTLChange{RData: r.RData, OldTTL: r.TTL, NewTTL: nr.TTL})
		} else {
			stillRemoved = append(stillRemoved, r)
		}
	}
	var stillAdded []*Record
	for _, cands := range addedByData {
		stillAdded = append(stillAdded, cands...)
	}

	sortRecs(stillRemoved)
	sortRecs(stillAdded)
	sort.Slice(rd.TTLChanges, func(i, j int) bool { return rd.TTLChanges[i].RData < rd.TTLChanges[j].RData })

	rd.Removed = stillRemoved
	rd.Added = stillAdded
	same := len(stillRemoved) == 0 && len(stillAdded) == 0 && len(rd.TTLChanges) == 0
	return rd, same
}

func sortRecs(recs []*Record) {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].RData != recs[j].RData {
			return recs[i].RData < recs[j].RData
		}
		return recs[i].TTL < recs[j].TTL
	})
}

// TypeString 返回类型助记名（供 API 层使用）。
func TypeString(t uint16) string {
	if s, ok := dns.TypeToString[t]; ok {
		return s
	}
	return "UNKNOWN"
}

// ClassString 返回类别助记名。
func ClassString(c uint16) string {
	if s, ok := dns.ClassToString[c]; ok {
		return s
	}
	return "UNKNOWN"
}
