package collector_test

import (
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"zonestation/internal/collector"
	"zonestation/internal/station"
	"zonestation/internal/testdns"
)

func setup(t *testing.T) (*testdns.Server, *collector.Service, *station.Store) {
	t.Helper()
	ts, err := testdns.New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动测试服务器失败: %v", err)
	}
	t.Cleanup(ts.Close)
	svc := &collector.Service{Addr: ts.Addr(), Timeout: 3 * time.Second}
	return ts, svc, station.New()
}

func mustOK(t *testing.T, st *station.Store, svc *collector.Service, zone string, mode collector.Mode) *collector.Result {
	t.Helper()
	res := st.Transfer(svc, zone, mode)
	if !res.Success {
		t.Fatalf("%s %s 应成功，实际被拒: %v", zone, mode, res.Rejections)
	}
	return res
}

func mustFail(t *testing.T, st *station.Store, svc *collector.Service, zone string, mode collector.Mode, wantSub string) *collector.Result {
	t.Helper()
	res := st.Transfer(svc, zone, mode)
	if res.Success {
		t.Fatalf("%s %s 应被拒收，实际成功", zone, mode)
	}
	for _, r := range res.Rejections {
		if strings.Contains(r, wantSub) {
			return res
		}
	}
	t.Fatalf("拒收原因 %v 中未找到 %q", res.Rejections, wantSub)
	return nil
}

func bump(t *testing.T, ts *testdns.Server, zone string) uint32 {
	t.Helper()
	info, err := ts.Bump(zone)
	if err != nil {
		t.Fatalf("推进 %s 失败: %v", zone, err)
	}
	return info.Serial
}

func TestAXFRBasic(t *testing.T) {
	_, svc, st := setup(t)
	res := mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	if res.NewSerial != 2024010101 {
		t.Fatalf("序列号应为 2024010101，实际 %d", res.NewSerial)
	}
	snap := st.Current("example.com.")
	if snap == nil || len(snap.Zone.Records) == 0 {
		t.Fatalf("成功后应有区快照")
	}
	// 全量带来的记录应标来源。
	for _, r := range snap.Zone.Records {
		if r.Source.TransferID != res.ID || r.Source.Kind != "full" {
			t.Fatalf("全量记录来源应为 传送 #%d 全量: %+v", res.ID, r.Source)
		}
	}
}

func TestIXFRWithoutLocalCopy(t *testing.T) {
	_, svc, st := setup(t)
	mustFail(t, st, svc, "example.com.", collector.ModeIXFR, "本地没有该区副本")
}

func TestIXFRMultiSegmentAcrossRevisions(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	bump(t, ts, "example.com.")
	bump(t, ts, "example.com.")
	bump(t, ts, "example.com.")

	res := mustOK(t, st, svc, "example.com.", collector.ModeIXFR)
	if len(res.Segments) != 3 {
		t.Fatalf("跨 3 次修订应有 3 个增量段: %+v", res.Segments)
	}
	if res.Segments[0].OldSerial != 2024010101 || res.Segments[2].NewSerial != 2024010104 {
		t.Fatalf("段序列号衔接不符: %+v", res.Segments)
	}
	if st.Current("example.com.").Zone.Serial != 2024010104 {
		t.Fatalf("当前序列号应为 2024010104")
	}

	// 对照：www 记录集应体现 TTL 改动 + 记录增删，而不是整个记录集被删。
	d := res.Diff
	if d == nil {
		t.Fatalf("应有传送前后对照")
	}
	for _, v := range d.Added {
		if v.Name == "www.example.com." {
			t.Fatalf("www 记录集不应出现在新增里（TTL 改动 ≠ 删除记录集）")
		}
	}
	for _, v := range d.Removed {
		if v.Name == "www.example.com." {
			t.Fatalf("www 记录集不应出现在删除里（TTL 改动 ≠ 删除记录集）")
		}
	}
	// 最终内容校验：www 剩 .80(600) 与 .82(3600)。
	sets := st.Current("example.com.").Zone.RRsets()
	var wwwTTLs []uint32
	for _, r := range st.Current("example.com.").Zone.Records {
		if r.Name == "www.example.com." {
			wwwTTLs = append(wwwTTLs, r.TTL)
		}
	}
	if len(wwwTTLs) != 2 {
		t.Fatalf("www 最终应有 2 条记录: %v", wwwTTLs)
	}
	if len(sets) == 0 {
		t.Fatalf("记录集不应为空")
	}
}

func TestTTLChangeWithinRRset(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	bump(t, ts, "example.com.") // v2
	mustOK(t, st, svc, "example.com.", collector.ModeIXFR)
	bump(t, ts, "example.com.") // v3：www .80 TTL 3600→600
	res := mustOK(t, st, svc, "example.com.", collector.ModeIXFR)

	found := false
	for _, m := range res.Diff.Modified {
		if m.Name == "www.example.com." {
			found = true
			if len(m.TTLChanges) != 1 || m.TTLChanges[0].OldTTL != 3600 || m.TTLChanges[0].NewTTL != 600 {
				t.Fatalf("www 应体现 TTL 3600→600 的集内修改: %+v", m.TTLChanges)
			}
		}
	}
	if !found {
		t.Fatalf("www 记录集应出现在修改列表: %+v", res.Diff)
	}
}

func TestAXFRFallbackRecognized(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "nohist.test.", collector.ModeAXFR)
	bump(t, ts, "nohist.test.")
	res := mustOK(t, st, svc, "nohist.test.", collector.ModeIXFR)
	if !res.FellBackToFull {
		t.Fatalf("服务器只肯给全量时应识别为全量回退")
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("全量回退应给出警告说明")
	}
	if st.Current("nohist.test.").Zone.Serial != 101 {
		t.Fatalf("回退后序列号应为 101")
	}
}

func TestMissingEndRejectedAndOldZoneKept(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	bump(t, ts, "example.com.")

	ts.SetBehaviors(testdns.Behaviors{MissingEnd: true})
	res := mustFail(t, st, svc, "example.com.", collector.ModeIXFR, "结束")
	if st.Current("example.com.").Zone.Serial != 2024010101 {
		t.Fatalf("失败后旧区应保持不变（序列号 2024010101）")
	}
	if len(res.Raw) == 0 {
		t.Fatalf("失败结果应保留实际收到的记录供检查")
	}
	// 全量同样被拒。
	mustFail(t, st, svc, "example.com.", collector.ModeAXFR, "结束")
	// 旧区一直可查询。
	if st.SnapshotAt("example.com.", 2024010101) == nil {
		t.Fatalf("旧区快照应一直可查询")
	}
	// 关掉开关后恢复。
	ts.SetBehaviors(testdns.Behaviors{})
	mustOK(t, st, svc, "example.com.", collector.ModeIXFR)
}

func TestDuplicateRecordsRejected(t *testing.T) {
	ts, svc, st := setup(t)
	ts.SetBehaviors(testdns.Behaviors{DuplicateRecords: true})
	mustFail(t, st, svc, "example.com.", collector.ModeAXFR, "重复")
	if st.Current("example.com.") != nil {
		t.Fatalf("失败不应产生快照")
	}
}

func TestDuplicateRecordsRejectedIXFR(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	bump(t, ts, "example.com.")
	ts.SetBehaviors(testdns.Behaviors{DuplicateRecords: true})
	res := mustFail(t, st, svc, "example.com.", collector.ModeIXFR, "重复")
	if st.Current("example.com.").Zone.Serial != 2024010101 {
		t.Fatalf("失败后旧区应保持不变")
	}
	if len(res.Raw) == 0 {
		t.Fatalf("失败结果应保留收到的数据")
	}
}

func TestRefusedTransfer(t *testing.T) {
	ts, svc, st := setup(t)
	ts.SetBehaviors(testdns.Behaviors{RefuseTransfer: true})
	mustFail(t, st, svc, "example.com.", collector.ModeAXFR, "REFUSED")

	// 拒绝传送不影响普通查询（旧区一直可查询）。
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeSOA)
	in, _, err := c.Exchange(m, ts.Addr())
	if err != nil || len(in.Answer) == 0 {
		t.Fatalf("拒绝传送时普通查询仍应应答: %v", err)
	}
}

func TestSerialWraparound(t *testing.T) {
	ts, svc, st := setup(t)
	ts.SetBehaviors(testdns.Behaviors{WrapSerial: true})

	res := mustOK(t, st, svc, "wrap.test.", collector.ModeAXFR)
	if res.NewSerial != 4294967290 {
		t.Fatalf("回绕模式起点序列号应为 4294967290，实际 %d", res.NewSerial)
	}
	bump(t, ts, "wrap.test.")
	bump(t, ts, "wrap.test.")
	bump(t, ts, "wrap.test.") // 4294967294 → 2 → 6

	res = mustOK(t, st, svc, "wrap.test.", collector.ModeIXFR)
	if res.NewSerial != 6 {
		t.Fatalf("回绕后序列号应为 6，实际 %d", res.NewSerial)
	}
	if res.SerialRelation != "less" {
		t.Fatalf("按 RFC 1982，4294967290 → 6 应判定为 less，实际 %s", res.SerialRelation)
	}
	if len(res.Segments) != 3 {
		t.Fatalf("回绕跨越 3 个修订段: %+v", res.Segments)
	}
}

func TestAmbiguousSerialStopsSwitch(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)

	ts.SetBehaviors(testdns.Behaviors{AmbiguousSerial: true})
	bump(t, ts, "example.com.") // 序列号跳变恰好 2^31

	res := mustFail(t, st, svc, "example.com.", collector.ModeIXFR, "2^31")
	if res.SerialRelation != "uncertain" {
		t.Fatalf("序列号关系应为 uncertain，实际 %s", res.SerialRelation)
	}
	// 全量同样停止切换。
	mustFail(t, st, svc, "example.com.", collector.ModeAXFR, "2^31")
	// 旧区保持不变。
	if st.Current("example.com.").Zone.Serial != 2024010101 {
		t.Fatalf("不确定时不得切换，旧区应保持 2024010101")
	}
	// 清除本地副本后可重新全量加载。
	st.ClearZone("example.com.")
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
}

func TestUpToDate(t *testing.T) {
	_, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	res := mustOK(t, st, svc, "example.com.", collector.ModeIXFR)
	if !res.UpToDate {
		t.Fatalf("无新版本时增量应报告已是最新")
	}
	if len(st.Snapshots("example.com.")) != 1 {
		t.Fatalf("已是最新不应产生新快照")
	}
}

func TestOldSnapshotsQueryable(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	bump(t, ts, "example.com.")
	mustOK(t, st, svc, "example.com.", collector.ModeIXFR)

	snaps := st.Snapshots("example.com.")
	if len(snaps) != 2 {
		t.Fatalf("应有 2 份历史快照，实际 %d", len(snaps))
	}
	old := st.SnapshotAt("example.com.", 2024010101)
	if old == nil {
		t.Fatalf("旧区快照应一直可查询")
	}
	// 旧区里 MX 的 TTL 还是 3600。
	for _, r := range old.Zone.Records {
		if r.Type == 15 && r.TTL != 3600 {
			t.Fatalf("旧区 MX 的 TTL 应为 3600，实际 %d", r.TTL)
		}
	}
}

func TestFailedResultsInspectableSeparately(t *testing.T) {
	ts, svc, st := setup(t)
	mustOK(t, st, svc, "example.com.", collector.ModeAXFR)
	ts.SetBehaviors(testdns.Behaviors{RefuseTransfer: true})
	mustFail(t, st, svc, "example.com.", collector.ModeAXFR, "REFUSED")

	failed := st.Results("", true)
	if len(failed) != 1 {
		t.Fatalf("应能单独列出 1 条失败结果，实际 %d", len(failed))
	}
	all := st.Results("", false)
	if len(all) != 2 {
		t.Fatalf("全部记录应有 2 条，实际 %d", len(all))
	}
}
