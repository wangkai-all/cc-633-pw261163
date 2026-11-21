// Package httpapi 提供采集站的 REST API 与网页静态资源服务。
package httpapi

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"zonestation/internal/collector"
	"zonestation/internal/station"
	"zonestation/internal/testdns"
	"zonestation/internal/zone"
)

//go:embed web
var webFS embed.FS

// API 聚合采集站各组件，暴露 HTTP 接口。
type API struct {
	TS    *testdns.Server
	Store *station.Store
	Svc   *collector.Service
}

// Handler 构造 HTTP 路由。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/overview", a.overview)
	mux.HandleFunc("PUT /api/server/behaviors", a.setBehaviors)
	mux.HandleFunc("POST /api/server/zones/{zone}/bump", a.bump)
	mux.HandleFunc("GET /api/zones/{zone}/snapshot", a.snapshot)
	mux.HandleFunc("GET /api/zones/{zone}/snapshots", a.snapshots)
	mux.HandleFunc("GET /api/zones/{zone}/snapshots/{serial}", a.snapshotAt)
	mux.HandleFunc("GET /api/zones/{zone}/download", a.download)
	mux.HandleFunc("POST /api/zones/{zone}/transfer", a.transfer)
	mux.HandleFunc("DELETE /api/zones/{zone}", a.clearZone)
	mux.HandleFunc("GET /api/transfers", a.transfers)
	mux.HandleFunc("GET /api/transfers/{id}", a.transferByID)
	mux.Handle("/", a.static())
	return mux
}

// ---------- DTO ----------

// RecordDTO 是记录的 JSON 视图（类别/类型用助记名）。
type RecordDTO struct {
	Name   string      `json:"name"`
	Class  string      `json:"class"`
	Type   string      `json:"type"`
	TTL    uint32      `json:"ttl"`
	RData  string      `json:"rdata"`
	Source zone.Source `json:"source"`
}

// RRsetDTO 是记录集的 JSON 视图；记录集以 （名称，类别，类型） 识别。
type RRsetDTO struct {
	Key     string      `json:"key"`
	Name    string      `json:"name"`
	Class   string      `json:"class"`
	Type    string      `json:"type"`
	TTLs    []uint32    `json:"ttls"`
	Records []RecordDTO `json:"records"`
}

// SnapshotDTO 是区快照的 JSON 视图。
type SnapshotDTO struct {
	Zone       string     `json:"zone"`
	Serial     uint32     `json:"serial"`
	TakenAt    time.Time  `json:"takenAt"`
	TransferID int64      `json:"transferId"`
	RRsets     []RRsetDTO `json:"rrsets"`
}

// RRsetDiffDTO 是记录集级对照的 JSON 视图。
type RRsetDiffDTO struct {
	Key        string           `json:"key"`
	Name       string           `json:"name"`
	Class      string           `json:"class"`
	Type       string           `json:"type"`
	Added      []RecordDTO      `json:"added"`
	Removed    []RecordDTO      `json:"removed"`
	TTLChanges []zone.TTLChange `json:"ttlChanges"`
}

// DiffDTO 是传送前后对照的 JSON 视图。
type DiffDTO struct {
	Added     []RRsetDTO     `json:"added"`
	Removed   []RRsetDTO     `json:"removed"`
	Modified  []RRsetDiffDTO `json:"modified"`
	Unchanged int            `json:"unchanged"`
}

// ResultDTO 是传送记录的 JSON 视图。
type ResultDTO struct {
	ID             int64                   `json:"id"`
	Zone           string                  `json:"zone"`
	Mode           collector.Mode          `json:"mode"`
	StartedAt      time.Time               `json:"startedAt"`
	FinishedAt     time.Time               `json:"finishedAt"`
	Success        bool                    `json:"success"`
	UpToDate       bool                    `json:"upToDate"`
	FellBackToFull bool                    `json:"fellBackToFull"`
	HadLocalCopy   bool                    `json:"hadLocalCopy"`
	OldSerial      uint32                  `json:"oldSerial"`
	NewSerial      uint32                  `json:"newSerial"`
	SerialRelation string                  `json:"serialRelation"`
	Segments       []collector.SegmentInfo `json:"segments"`
	Rejections     []string                `json:"rejections"`
	Warnings       []string                `json:"warnings"`
	ReceivedCount  int                     `json:"receivedCount"`
	Raw            []string                `json:"raw,omitempty"`
	Diff           *DiffDTO                `json:"diff,omitempty"`
}

// OverviewDTO 是首页概览。
type OverviewDTO struct {
	ServerAddr  string                `json:"serverAddr"`
	Now         time.Time             `json:"now"`
	Behaviors   testdns.Behaviors     `json:"behaviors"`
	ServerZones []testdns.ZoneInfo    `json:"serverZones"`
	Station     []station.ZoneSummary `json:"station"`
}

// ---------- 转换 ----------

func toRecordDTO(r *zone.Record) RecordDTO {
	return RecordDTO{
		Name:   r.Name,
		Class:  zone.ClassString(r.Class),
		Type:   zone.TypeString(r.Type),
		TTL:    r.TTL,
		RData:  r.RData,
		Source: r.Source,
	}
}

func toRRsetDTO(recs []*zone.Record) RRsetDTO {
	sorted := make([]*zone.Record, len(recs))
	copy(sorted, recs)
	zone.SortRecords(sorted)
	d := RRsetDTO{Key: sorted[0].SetKey(), Name: sorted[0].Name,
		Class: zone.ClassString(sorted[0].Class), Type: zone.TypeString(sorted[0].Type),
		TTLs: []uint32{}, Records: []RecordDTO{}}
	ttlSeen := map[uint32]bool{}
	for _, r := range sorted {
		if !ttlSeen[r.TTL] {
			ttlSeen[r.TTL] = true
			d.TTLs = append(d.TTLs, r.TTL)
		}
		d.Records = append(d.Records, toRecordDTO(r))
	}
	sort.Slice(d.TTLs, func(i, j int) bool { return d.TTLs[i] < d.TTLs[j] })
	return d
}

func toSnapshotDTO(snap *station.Snapshot) SnapshotDTO {
	z := snap.Zone
	d := SnapshotDTO{Zone: z.Origin, Serial: z.Serial, TakenAt: snap.TakenAt, TransferID: snap.TransferID, RRsets: []RRsetDTO{}}
	sets := z.RRsets()
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.RRsets = append(d.RRsets, toRRsetDTO(sets[k]))
	}
	// 记录集按规范名称顺序排列。
	sort.Slice(d.RRsets, func(i, j int) bool {
		a, b := d.RRsets[i], d.RRsets[j]
		if a.Name != b.Name {
			return zone.CompareName(a.Name, b.Name) < 0
		}
		return a.Type < b.Type
	})
	return d
}

func toDiffDTO(d *zone.Diff) *DiffDTO {
	if d == nil {
		return nil
	}
	out := &DiffDTO{
		Unchanged: d.Unchanged,
		Added:     []RRsetDTO{},
		Removed:   []RRsetDTO{},
		Modified:  []RRsetDiffDTO{},
	}
	for _, v := range d.Added {
		out.Added = append(out.Added, toRRsetDTO(v.Records))
	}
	for _, v := range d.Removed {
		out.Removed = append(out.Removed, toRRsetDTO(v.Records))
	}
	for _, m := range d.Modified {
		md := RRsetDiffDTO{
			Key:        m.Key,
			Name:       m.Name,
			Class:      zone.ClassString(m.Class),
			Type:       zone.TypeString(m.Type),
			TTLChanges: m.TTLChanges,
			Added:      []RecordDTO{},
			Removed:    []RecordDTO{},
		}
		if md.TTLChanges == nil {
			md.TTLChanges = []zone.TTLChange{}
		}
		for _, r := range m.Added {
			md.Added = append(md.Added, toRecordDTO(r))
		}
		for _, r := range m.Removed {
			md.Removed = append(md.Removed, toRecordDTO(r))
		}
		out.Modified = append(out.Modified, md)
	}
	return out
}

func toResultDTO(r *collector.Result, full bool) ResultDTO {
	d := ResultDTO{
		ID: r.ID, Zone: r.Zone, Mode: r.Mode,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		Success: r.Success, UpToDate: r.UpToDate, FellBackToFull: r.FellBackToFull,
		HadLocalCopy: r.HadLocalCopy,
		OldSerial:    r.OldSerial, NewSerial: r.NewSerial,
		SerialRelation: r.SerialRelation,
		Segments:       r.Segments, Rejections: r.Rejections, Warnings: r.Warnings,
		ReceivedCount: r.ReceivedCount,
	}
	if d.Segments == nil {
		d.Segments = []collector.SegmentInfo{}
	}
	if d.Rejections == nil {
		d.Rejections = []string{}
	}
	if d.Warnings == nil {
		d.Warnings = []string{}
	}
	if full {
		d.Raw = r.Raw
		d.Diff = toDiffDTO(r.Diff)
	}
	return d
}

// ---------- 处理器 ----------

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "now": time.Now()})
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, OverviewDTO{
		ServerAddr:  a.TS.Addr(),
		Now:         time.Now(),
		Behaviors:   a.TS.Behaviors(),
		ServerZones: a.TS.ZoneInfos(),
		Station:     a.Store.Summaries(),
	})
}

func (a *API) setBehaviors(w http.ResponseWriter, r *http.Request) {
	var b testdns.Behaviors
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "行为开关 JSON 解析失败: %v", err)
		return
	}
	a.TS.SetBehaviors(b)
	writeJSON(w, http.StatusOK, a.TS.Behaviors())
}

func (a *API) bump(w http.ResponseWriter, r *http.Request) {
	info, err := a.TS.Bump(r.PathValue("zone"))
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (a *API) snapshot(w http.ResponseWriter, r *http.Request) {
	snap := a.Store.Current(r.PathValue("zone"))
	if snap == nil {
		writeError(w, http.StatusNotFound, "本地没有该区副本，请先发起全量传送")
		return
	}
	writeJSON(w, http.StatusOK, toSnapshotDTO(snap))
}

func (a *API) snapshots(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Serial     uint32    `json:"serial"`
		TakenAt    time.Time `json:"takenAt"`
		TransferID int64     `json:"transferId"`
	}
	out := []item{}
	for _, snap := range a.Store.Snapshots(r.PathValue("zone")) {
		out = append(out, item{Serial: snap.Zone.Serial, TakenAt: snap.TakenAt, TransferID: snap.TransferID})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) snapshotAt(w http.ResponseWriter, r *http.Request) {
	serial, err := strconv.ParseUint(r.PathValue("serial"), 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "序列号格式不正确")
		return
	}
	snap := a.Store.SnapshotAt(r.PathValue("zone"), uint32(serial))
	if snap == nil {
		writeError(w, http.StatusNotFound, "没有序列号为 %d 的历史快照", serial)
		return
	}
	writeJSON(w, http.StatusOK, toSnapshotDTO(snap))
}

func (a *API) download(w http.ResponseWriter, r *http.Request) {
	origin := r.PathValue("zone")
	var snap *station.Snapshot
	if s := r.URL.Query().Get("serial"); s != "" {
		serial, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, "序列号格式不正确")
			return
		}
		snap = a.Store.SnapshotAt(origin, uint32(serial))
	} else {
		snap = a.Store.Current(origin)
	}
	if snap == nil {
		writeError(w, http.StatusNotFound, "本地没有该区副本，请先发起传送")
		return
	}
	filename := zone.Canonical(origin) + "zone"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	_, _ = w.Write([]byte(snap.Zone.MasterFile()))
}

type transferRequest struct {
	Mode string `json:"mode"`
}

func (a *API) transfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求 JSON 解析失败: %v", err)
		return
	}
	var mode collector.Mode
	switch req.Mode {
	case string(collector.ModeAXFR):
		mode = collector.ModeAXFR
	case string(collector.ModeIXFR):
		mode = collector.ModeIXFR
	default:
		writeError(w, http.StatusBadRequest, "mode 必须是 axfr 或 ixfr")
		return
	}
	res := a.Store.Transfer(a.Svc, r.PathValue("zone"), mode)
	writeJSON(w, http.StatusOK, toResultDTO(res, true))
}

func (a *API) clearZone(w http.ResponseWriter, r *http.Request) {
	a.Store.ClearZone(r.PathValue("zone"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

func (a *API) transfers(w http.ResponseWriter, r *http.Request) {
	failedOnly := r.URL.Query().Get("failed") == "1" || r.URL.Query().Get("failed") == "true"
	list := a.Store.Results(r.URL.Query().Get("zone"), failedOnly)
	out := []ResultDTO{}
	for _, res := range list {
		out = append(out, toResultDTO(res, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) transferByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "传送记录 ID 格式不正确")
		return
	}
	res := a.Store.Result(id)
	if res == nil {
		writeError(w, http.StatusNotFound, "没有编号为 %d 的传送记录", id)
		return
	}
	writeJSON(w, http.StatusOK, toResultDTO(res, true))
}

func (a *API) static() http.Handler {
	if dir := os.Getenv("ZONESTATION_WEB_DIR"); dir != "" {
		return http.FileServer(http.Dir(dir))
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}
