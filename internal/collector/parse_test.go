package collector

import (
	"strconv"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"zonestation/internal/zone"
)

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("解析记录失败 %q: %v", s, err)
	}
	return rr
}

func soaRR(t *testing.T, serial uint32) dns.RR {
	t.Helper()
	return mustRR(t, "example.com. 300 IN SOA ns1.example.com. admin.example.com. "+
		strconv.FormatUint(uint64(serial), 10)+" 7200 3600 1209600 300")
}

func TestParseFullStream(t *testing.T) {
	rrs := []dns.RR{
		soaRR(t, 9),
		mustRR(t, "example.com. 300 IN NS ns1.example.com."),
		mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"),
		soaRR(t, 9),
	}
	p, err := ParseStream("example.com.", ModeAXFR, rrs)
	if err != nil {
		t.Fatalf("全量流应解析成功: %v", err)
	}
	if p.Format != FormatFull || p.NewSerial != 9 || len(p.Full) != 3 {
		t.Fatalf("解析结果不符: %+v", p)
	}
}

func TestParseFullMissingEndSOA(t *testing.T) {
	rrs := []dns.RR{
		soaRR(t, 9),
		mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"),
	}
	if _, err := ParseStream("example.com.", ModeAXFR, rrs); err == nil ||
		!strings.Contains(err.Error(), "结束 SOA") {
		t.Fatalf("缺少结束 SOA 应报错: %v", err)
	}
}

func TestParseWrongZoneSOA(t *testing.T) {
	rrs := []dns.RR{
		mustRR(t, "other.com. 300 IN SOA ns1.other.com. admin.other.com. 9 7200 3600 1209600 300"),
		mustRR(t, "other.com. 300 IN NS ns1.other.com."),
		mustRR(t, "other.com. 300 IN SOA ns1.other.com. admin.other.com. 9 7200 3600 1209600 300"),
	}
	if _, err := ParseStream("example.com.", ModeAXFR, rrs); err == nil ||
		!strings.Contains(err.Error(), "区归属") {
		t.Fatalf("外区 SOA 应报区归属错误: %v", err)
	}
}

func TestParseDeltaMultiSegment(t *testing.T) {
	// 两段增量：1→2、2→3，跨多次修订。
	rrs := []dns.RR{
		soaRR(t, 3), // 响应首条：宣告最新序列号
		soaRR(t, 1), // 段1 旧 SOA
		mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"),
		soaRR(t, 2), // 段1 新 SOA
		mustRR(t, "www.example.com. 600 IN A 192.0.2.80"),
		soaRR(t, 2), // 段2 旧 SOA
		mustRR(t, "old.example.com. 300 IN TXT \"x\""),
		soaRR(t, 3), // 段2 新 SOA
		mustRR(t, "api.example.com. 300 IN A 192.0.2.90"),
	}
	p, err := ParseStream("example.com.", ModeIXFR, rrs)
	if err != nil {
		t.Fatalf("多段增量应解析成功: %v", err)
	}
	if p.Format != FormatDelta || len(p.Deltas) != 2 {
		t.Fatalf("应解析出 2 个修订段: %+v", p)
	}
	if p.Segments[0].OldSerial != 1 || p.Segments[0].NewSerial != 2 ||
		p.Segments[1].OldSerial != 2 || p.Segments[1].NewSerial != 3 {
		t.Fatalf("段序列号不符: %+v", p.Segments)
	}
	if p.Segments[0].Deleted != 1 || p.Segments[0].Added != 1 ||
		p.Segments[1].Deleted != 1 || p.Segments[1].Added != 1 {
		t.Fatalf("段内增删计数不符: %+v", p.Segments)
	}
}

func TestParseDeltaMissingEndSegment(t *testing.T) {
	// 末段缺少新 SOA 与增加项。
	rrs := []dns.RR{
		soaRR(t, 3),
		soaRR(t, 1),
		mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"),
		soaRR(t, 2),
		mustRR(t, "www.example.com. 600 IN A 192.0.2.80"),
		soaRR(t, 2),
		mustRR(t, "old.example.com. 300 IN TXT \"x\""),
		// 这里本应有 soaRR(3) 与增加项
	}
	_, err := ParseStream("example.com.", ModeIXFR, rrs)
	if err == nil || !strings.Contains(err.Error(), "结束") {
		t.Fatalf("缺失结束段应报错: %v", err)
	}
}

func TestParseDeltaAmbiguousSerial(t *testing.T) {
	const half = uint32(1) << 31
	rrs := []dns.RR{
		soaRR(t, 1+half),
		soaRR(t, 1),
		mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"),
		soaRR(t, 1+half),
		mustRR(t, "www.example.com. 600 IN A 192.0.2.80"),
	}
	_, err := ParseStream("example.com.", ModeIXFR, rrs)
	if err == nil || !strings.Contains(err.Error(), "2^31") {
		t.Fatalf("差值 2^31 的段应报不确定错误: %v", err)
	}
}

func TestParseDeltaWrapSerialOK(t *testing.T) {
	// 序列号回绕：4294967294 → 2，按 RFC 1982 是递增。
	rrs := []dns.RR{
		soaRR(t, 2),
		soaRR(t, 4294967294),
		mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"),
		soaRR(t, 2),
		mustRR(t, "www.example.com. 600 IN A 192.0.2.80"),
	}
	p, err := ParseStream("example.com.", ModeIXFR, rrs)
	if err != nil {
		t.Fatalf("回绕序列号的段应解析成功: %v", err)
	}
	if p.Segments[0].OldSerial != 4294967294 || p.Segments[0].NewSerial != 2 {
		t.Fatalf("段序列号不符: %+v", p.Segments[0])
	}
}

func TestParseIXFRFullFallback(t *testing.T) {
	// 请求 IXFR，但第二条不是 SOA：服务器只肯给全量。
	rrs := []dns.RR{
		soaRR(t, 5),
		mustRR(t, "example.com. 300 IN NS ns1.example.com."),
		soaRR(t, 5),
	}
	p, err := ParseStream("example.com.", ModeIXFR, rrs)
	if err != nil {
		t.Fatalf("全量回退应解析成功: %v", err)
	}
	if p.Format != FormatFull {
		t.Fatalf("应识别为全量格式: %+v", p)
	}
}

func TestParseUpToDate(t *testing.T) {
	p, err := ParseStream("example.com.", ModeIXFR, []dns.RR{soaRR(t, 5)})
	if err != nil {
		t.Fatalf("单 SOA 响应应解析成功: %v", err)
	}
	if p.Format != FormatUpToDate {
		t.Fatalf("应识别为已是最新: %+v", p)
	}
}

func TestApplyDeltaTTLChangeKeepsRRset(t *testing.T) {
	local := &zone.Zone{Origin: "example.com.", Serial: 1}
	for _, s := range []string{
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 300",
		"www.example.com. 3600 IN A 192.0.2.80",
		"www.example.com. 3600 IN A 192.0.2.81",
	} {
		local.Records = append(local.Records, zone.FromRR(mustRR(t, s), zone.Source{TransferID: 1, Kind: "full"}))
	}
	mkSOA := func(serial uint32) *dns.SOA {
		return soaRR(t, serial).(*dns.SOA)
	}
	deltas := []Delta{{
		OldSOA: mkSOA(1),
		NewSOA: mkSOA(2),
		Del:    []dns.RR{mustRR(t, "www.example.com. 3600 IN A 192.0.2.80")},
		Add:    []dns.RR{mustRR(t, "www.example.com. 600 IN A 192.0.2.80")},
	}}
	out, warns, errs := ApplyDelta(local, deltas, 7)
	if len(errs) != 0 {
		t.Fatalf("应用增量段不应出错: %v", errs)
	}
	if len(warns) != 0 {
		t.Fatalf("不应有警告: %v", warns)
	}
	// 记录集必须保留：www A 仍有两条记录，其中 .80 的 TTL 变为 600。
	sets := out.RRsets()
	www := sets[zone.SetKey("www.example.com.", 1, 1)]
	if len(www) != 2 {
		t.Fatalf("TTL 改动后记录集应仍有 2 条记录: %+v", www)
	}
	var ttl80 uint32
	for _, r := range www {
		if strings.Contains(r.RData, "192.0.2.80") {
			ttl80 = r.TTL
		}
	}
	if ttl80 != 600 {
		t.Fatalf(".80 的 TTL 应为 600，实际 %d", ttl80)
	}
	if out.Serial != 2 {
		t.Fatalf("区序列号应为 2，实际 %d", out.Serial)
	}
	// 新记录带来源信息。
	found := false
	for _, r := range www {
		if r.Source.TransferID == 7 && r.Source.Kind == "delta" && r.Source.Segment == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("新增记录应带来源（传送 #7 增量段 1）: %+v", www)
	}
}

func TestApplyDeltaDuplicateRejected(t *testing.T) {
	local := &zone.Zone{Origin: "example.com.", Serial: 1}
	local.Records = append(local.Records,
		zone.FromRR(soaRR(t, 1), zone.Source{}),
		zone.FromRR(mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"), zone.Source{}),
	)
	deltas := []Delta{{
		OldSOA: soaRR(t, 1).(*dns.SOA),
		NewSOA: soaRR(t, 2).(*dns.SOA),
		Add:    []dns.RR{mustRR(t, "www.example.com. 3600 IN A 192.0.2.80")}, // 已存在
	}}
	_, _, errs := ApplyDelta(local, deltas, 7)
	if len(errs) == 0 || !strings.Contains(errs[0], "重复") {
		t.Fatalf("重复新增应报错: %v", errs)
	}
}

func TestValidateZone(t *testing.T) {
	good := &zone.Zone{Origin: "example.com.", Serial: 3}
	for _, s := range []string{
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 300",
		"www.example.com. 3600 IN A 192.0.2.80",
	} {
		good.Records = append(good.Records, zone.FromRR(mustRR(t, s), zone.Source{}))
	}
	if errs, _ := ValidateZone(good, 3); len(errs) != 0 {
		t.Fatalf("合法区不应有校验错误: %v", errs)
	}

	// 区归属：混入外区记录。
	bad := &zone.Zone{Origin: "example.com.", Serial: 3}
	bad.Records = append(bad.Records,
		zone.FromRR(soaRR(t, 3), zone.Source{}),
		zone.FromRR(mustRR(t, "www.other.com. 3600 IN A 192.0.2.80"), zone.Source{}),
	)
	if errs, _ := ValidateZone(bad, 3); len(errs) == 0 {
		t.Fatalf("外区记录应被区归属校验拦下")
	}

	// 记录集一致性：同一记录集 TTL 不一致 → 警告而非拒收（RFC 2181）。
	bad2 := &zone.Zone{Origin: "example.com.", Serial: 3}
	bad2.Records = append(bad2.Records,
		zone.FromRR(soaRR(t, 3), zone.Source{}),
		zone.FromRR(mustRR(t, "www.example.com. 3600 IN A 192.0.2.80"), zone.Source{}),
		zone.FromRR(mustRR(t, "www.example.com. 600 IN A 192.0.2.81"), zone.Source{}),
	)
	errs, warns := ValidateZone(bad2, 3)
	if len(errs) != 0 {
		t.Fatalf("TTL 不一致不应拒收: %v", errs)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "TTL 不一致") {
			found = true
		}
	}
	if !found {
		t.Fatalf("TTL 不一致应给出警告: %v", warns)
	}
}
