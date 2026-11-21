package collector

import (
	"fmt"

	"github.com/miekg/dns"

	"zonestation/internal/zone"
)

// BuildFromFull 把全量格式的记录装配为区快照，并做记录级校验（重复记录等）。
func BuildFromFull(origin string, rrs []dns.RR, txID int64) (*zone.Zone, []string) {
	var errs []string
	z := &zone.Zone{Origin: origin}
	seen := map[string]bool{}
	src := zone.Source{TransferID: txID, Kind: "full"}
	for _, rr := range rrs {
		rec := zone.FromRR(rr, src)
		if rec.Type == dns.TypeSOA {
			if zone.Canonical(rr.Header().Name) != origin {
				errs = append(errs, fmt.Sprintf("SOA 出现在非顶点名称 %s", rec.Name))
				continue
			}
			if soa, ok := rr.(*dns.SOA); ok {
				z.Serial = soa.Serial
			}
		}
		id := rec.Identity()
		if seen[id] {
			errs = append(errs, fmt.Sprintf("传送中包含重复记录（记录集一致性校验失败）：%s", rr.String()))
			continue
		}
		seen[id] = true
		z.Records = append(z.Records, rec)
	}
	return z, errs
}

// ApplyDelta 把若干增量修订段依次应用到旧区副本上。
//
// 删除与增加都按整条记录进行；记录集只以 （名称，类别，类型） 识别，
// 因此一条记录的 TTL 改动（删旧 TTL、增新 TTL）只是记录集内部变化，
// 记录集本身始终保留。
func ApplyDelta(local *zone.Zone, deltas []Delta, txID int64) (*zone.Zone, []string, []string) {
	var warns, errs []string
	cur := local.Clone()

	for i, d := range deltas {
		seg := i + 1
		for _, rr := range d.Del {
			rec := zone.FromRR(rr, zone.Source{})
			if cur.Remove(func(r *zone.Record) bool { return r.Identity() == rec.Identity() }) {
				continue
			}
			// 容错：TTL 不一致时按 （名称，类别，类型，RDATA) 匹配删除。
			if cur.Remove(func(r *zone.Record) bool { return r.DataIdentity() == rec.DataIdentity() }) {
				warns = append(warns, fmt.Sprintf("增量段 %d：删除 %s 时 TTL 与本地不一致，已按记录数据匹配删除", seg, rec.RData))
				continue
			}
			errs = append(errs, fmt.Sprintf("增量段 %d：要删除的记录在旧区中不存在（记录集一致性校验失败）：%s", seg, rr.String()))
		}
		for _, rr := range d.Add {
			rec := zone.FromRR(rr, zone.Source{TransferID: txID, Kind: "delta", Segment: seg})
			if cur.HasIdentity(rec.Identity()) {
				errs = append(errs, fmt.Sprintf("增量段 %d：新增记录与现有记录重复（记录集一致性校验失败）：%s", seg, rr.String()))
				continue
			}
			cur.Records = append(cur.Records, rec)
		}
	}

	// 用末段新 SOA 替换区 SOA。
	last := deltas[len(deltas)-1]
	finalSOA := zone.FromRR(last.NewSOA, zone.Source{TransferID: txID, Kind: "delta", Segment: len(deltas)})
	for cur.Remove(func(r *zone.Record) bool { return r.Type == dns.TypeSOA }) {
	}
	cur.Records = append([]*zone.Record{finalSOA}, cur.Records...)
	cur.Serial = last.NewSOA.Serial
	return cur, warns, errs
}

// ValidateZone 对装配完成的新区做整体校验：
// 唯一顶点 SOA 且序列号符合预期、所有记录归属本区、类别为 IN、
// 无重复记录——这些作为拒收原因返回。
// 记录集内 TTL 不一致按 RFC 2181 属于“不推荐但可接受”，
// 作为警告返回（单条记录的 TTL 改动不应导致整个记录集被拒）。
func ValidateZone(z *zone.Zone, expectSerial uint32) (errs, warns []string) {

	var soas []*zone.Record
	for _, r := range z.Records {
		if r.Type == dns.TypeSOA {
			soas = append(soas, r)
		}
	}
	if len(soas) == 0 {
		errs = append(errs, "新区中缺少 SOA 记录")
	} else {
		if len(soas) > 1 {
			errs = append(errs, fmt.Sprintf("新区中出现 %d 条 SOA 记录", len(soas)))
		}
		soa := soas[0]
		if soa.Name != z.Origin {
			errs = append(errs, fmt.Sprintf("SOA 不在区顶点（%s，应为 %s）", soa.Name, z.Origin))
		}
		if got, ok := soa.SerialOf(); ok && got != expectSerial {
			errs = append(errs, fmt.Sprintf("新区 SOA 序列号 %d 与传送宣告的 %d 不一致", got, expectSerial))
		}
	}

	for _, r := range z.Records {
		if !dns.IsSubDomain(z.Origin, r.Name) {
			errs = append(errs, fmt.Sprintf("记录 %s 不属于区 %s（区归属校验失败）", r.Name, z.Origin))
		}
		if r.Class != dns.ClassINET {
			errs = append(errs, fmt.Sprintf("记录 %s 的类别不是 IN（区归属校验失败）", r.Name))
		}
	}

	// 记录集一致性：同一 （名称，类别，类型） 内不得有重复记录；
	// TTL 不一致只作警告（RFC 2181 不推荐，但不构成拒收理由）。
	sets := z.RRsets()
	for key, recs := range sets {
		ttl := recs[0].TTL
		seen := map[string]bool{}
		for _, r := range recs {
			if r.TTL != ttl {
				warns = append(warns, fmt.Sprintf("记录集 %s 内 TTL 不一致（%d 与 %d），按 RFC 2181 不推荐，已接受",
					rrsetLabel(r), r.TTL, ttl))
			}
			id := r.Identity()
			if seen[id] {
				errs = append(errs, fmt.Sprintf("记录集 %s 内存在重复记录（记录集一致性校验失败）", rrsetLabel(r)))
			}
			seen[id] = true
		}
		_ = key
	}
	return errs, warns
}

func rrsetLabel(r *zone.Record) string {
	return fmt.Sprintf("%s %s %s", r.Name, zone.ClassString(r.Class), zone.TypeString(r.Type))
}
