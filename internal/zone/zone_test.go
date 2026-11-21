package zone

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func mkZone(serial uint32, lines ...string) *Zone {
	z := &Zone{Origin: "example.com.", Serial: serial}
	for _, l := range lines {
		rr, err := dns.NewRR(l)
		if err != nil {
			panic(err)
		}
		z.Records = append(z.Records, FromRR(rr, Source{}))
	}
	return z
}

func TestDiffTTLChangeIsNotRRsetDeletion(t *testing.T) {
	oldZ := mkZone(1,
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 300",
		"www.example.com. 3600 IN A 192.0.2.80",
		"www.example.com. 3600 IN A 192.0.2.81",
	)
	newZ := mkZone(2,
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 300",
		"www.example.com. 600 IN A 192.0.2.80", // 仅 TTL 改动
		"www.example.com. 3600 IN A 192.0.2.81",
	)
	d := DiffZones(oldZ, newZ)
	if len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("TTL 改动被误判为记录集增删: added=%d removed=%d", len(d.Added), len(d.Removed))
	}
	// SOA 与 www 两个记录集都被修改（SOA 序列号变了）。
	var www *RRsetDiff
	for i := range d.Modified {
		if d.Modified[i].Type == dns.TypeA {
			www = &d.Modified[i]
		}
	}
	if www == nil {
		t.Fatalf("www 记录集应显示为修改而不是增删: %+v", d)
	}
	if len(www.TTLChanges) != 1 || www.TTLChanges[0].OldTTL != 3600 || www.TTLChanges[0].NewTTL != 600 {
		t.Fatalf("应配对出一条 TTL 改动 3600→600: %+v", www.TTLChanges)
	}
	if len(www.Added) != 0 || len(www.Removed) != 0 {
		t.Fatalf("TTL 改动不应产生记录级增删: %+v", www)
	}
}

func TestDiffRRsetAddRemoveAndRecordChange(t *testing.T) {
	oldZ := mkZone(1,
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 300",
		"www.example.com. 3600 IN A 192.0.2.80",
		"www.example.com. 3600 IN A 192.0.2.81",
		"old.example.com. 300 IN TXT \"bye\"",
	)
	newZ := mkZone(2,
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 300",
		"www.example.com. 3600 IN A 192.0.2.80",
		"www.example.com. 3600 IN A 192.0.2.82", // .81 → .82：记录级一删一增
		"api.example.com. 300 IN A 192.0.2.90",  // 新记录集
	)
	d := DiffZones(oldZ, newZ)
	if len(d.Added) != 1 || d.Added[0].Name != "api.example.com." {
		t.Fatalf("应新增 api 记录集: %+v", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0].Name != "old.example.com." {
		t.Fatalf("应删除 old 记录集: %+v", d.Removed)
	}
	var www *RRsetDiff
	for i := range d.Modified {
		if d.Modified[i].Type == dns.TypeA && d.Modified[i].Name == "www.example.com." {
			www = &d.Modified[i]
		}
	}
	if www == nil {
		t.Fatalf("www 记录集应显示为修改: %+v", d.Modified)
	}
	if len(www.Added) != 1 || !strings.Contains(www.Added[0].RData, "192.0.2.82") {
		t.Fatalf("www 应新增 .82 记录: %+v", www.Added)
	}
	if len(www.Removed) != 1 || !strings.Contains(www.Removed[0].RData, "192.0.2.81") {
		t.Fatalf("www 应删除 .81 记录: %+v", www.Removed)
	}
	if len(www.TTLChanges) != 0 {
		t.Fatalf("RDATA 不同的增删不应配对为 TTL 改动: %+v", www.TTLChanges)
	}
}

func TestMasterFileNormalization(t *testing.T) {
	z := mkZone(7,
		"www.example.com. 3600 IN A 192.0.2.81",
		"example.com. 300 IN SOA ns1.example.com. admin.example.com. 7 7200 3600 1209600 300",
		"example.com. 300 IN NS ns2.example.com.",
		"example.com. 300 IN NS ns1.example.com.",
		"www.example.com. 3600 IN A 192.0.2.80",
		"api.example.com. 300 IN A 192.0.2.90",
	)
	out := z.MasterFile()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "$ORIGIN example.com." {
		t.Fatalf("缺少 $ORIGIN: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "$TTL 300") {
		t.Fatalf("$TTL 应取 SOA minimum: %q", lines[1])
	}
	// 规范顺序：SOA 最先；同一名称下 NS 在 A 前不可知，但顶点记录应连续；
	// 子名按规范名称排序（api < www）；同一记录集内按 RDATA 排序。
	body := strings.Join(lines[2:], "\n")
	if !strings.HasPrefix(body, "@\t300\tIN\tSOA") {
		t.Fatalf("SOA 应排在最前:\n%s", body)
	}
	apiIdx := strings.Index(body, "api")
	wwwIdx := strings.Index(body, "www")
	if apiIdx < 0 || wwwIdx < 0 || apiIdx > wwwIdx {
		t.Fatalf("名称应按规范顺序排列（api 在 www 前）:\n%s", body)
	}
	r80 := strings.Index(body, "192.0.2.80")
	r81 := strings.Index(body, "192.0.2.81")
	if r80 > r81 {
		t.Fatalf("同一记录集内应按 RDATA 排序:\n%s", body)
	}
}
