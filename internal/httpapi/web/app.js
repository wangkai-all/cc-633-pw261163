/* DNS 区数据采集站 —— 前端逻辑（Vue 3 全局构建，无打包步骤） */
(function () {
  const { createApp } = Vue;

  // ---------- 工具 ----------
  async function api(path, opts) {
    const res = await fetch(path, opts);
    let body = null;
    try { body = await res.json(); } catch (e) { /* 非 JSON */ }
    if (!res.ok) {
      throw new Error((body && body.error) || ('HTTP ' + res.status));
    }
    return body;
  }

  function fmtTime(t) {
    if (!t) return '-';
    const d = new Date(t);
    const p = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  }

  const RELATIONS = {
    less: '新区更新（RFC 1982）',
    equal: '序列号相同',
    greater: '新区更旧（RFC 1982）',
    uncertain: '不确定（差值 2^31）',
    unknown: '无本地副本',
  };

  function sourceLabel(src) {
    if (!src || !src.transferId) return '未知来源';
    if (src.kind === 'delta' && src.segment) return `传送 #${src.transferId} · 增量段 ${src.segment}`;
    return `传送 #${src.transferId} · 全量`;
  }

  // ---------- 记录集表格（可展开每条记录的来源） ----------
  const RrsetTable = {
    props: ['rrsets', 'transfers'],
    data() { return { open: {} }; },
    methods: {
      toggle(key) { this.open[key] = !this.open[key]; },
      sourceLabel,
      transferTime(id) {
        const t = (this.transfers || []).find((x) => x.id === id);
        return t ? fmtTime(t.startedAt) : '';
      },
    },
    template: `
      <table class="tbl rrset">
        <thead>
          <tr><th class="exp"></th><th>名称</th><th>类别</th><th>类型</th><th>TTL</th><th>记录数</th></tr>
        </thead>
        <tbody v-for="s in rrsets" :key="s.key">
          <tr class="rrset-row" @click="toggle(s.key)">
            <td class="exp">{{ open[s.key] ? '▾' : '▸' }}</td>
            <td class="mono">{{ s.name }}</td>
            <td>{{ s.class }}</td>
            <td><b>{{ s.type }}</b></td>
            <td>{{ s.ttls.join(' / ') }}</td>
            <td>{{ s.records.length }}</td>
          </tr>
          <tr v-if="open[s.key]" class="detail-row">
            <td></td>
            <td colspan="5">
              <table class="tbl inner">
                <thead><tr><th>RDATA</th><th>TTL</th><th>来源</th><th>采集时间</th></tr></thead>
                <tbody>
                  <tr v-for="(r, i) in s.records" :key="i">
                    <td class="mono">{{ r.rdata }}</td>
                    <td>{{ r.ttl }}</td>
                    <td><span class="tag src">{{ sourceLabel(r.source) }}</span></td>
                    <td class="muted">{{ transferTime(r.source.transferId) }}</td>
                  </tr>
                </tbody>
              </table>
            </td>
          </tr>
        </tbody>
      </table>
    `,
  };

  // ---------- 传送记录列表 ----------
  const TransferList = {
    props: ['transfers', 'selectedId'],
    emits: ['select'],
    methods: {
      fmtTime,
      resultText(t) {
        if (!t.success) return '失败';
        if (t.upToDate) return '已是最新';
        if (t.fellBackToFull) return '成功（服务器只给全量）';
        return '成功';
      },
      resultClass(t) {
        if (!t.success) return 'bad';
        if (t.upToDate) return '';
        if (t.fellBackToFull) return 'warn';
        return 'ok';
      },
      serialText(t) {
        if (!t.hadLocalCopy) return '→ ' + t.newSerial;
        if (t.oldSerial === t.newSerial) return String(t.newSerial);
        return t.oldSerial + ' → ' + t.newSerial;
      },
    },
    template: `
      <table class="tbl">
        <thead>
          <tr><th>#</th><th>时间</th><th>区</th><th>方式</th><th>结果</th><th>序列号</th><th>说明</th></tr>
        </thead>
        <tbody>
          <tr v-for="t in transfers" :key="t.id" class="clickable" :class="{on: selectedId === t.id}" @click="$emit('select', t.id)">
            <td>{{ t.id }}</td>
            <td class="muted">{{ fmtTime(t.startedAt) }}</td>
            <td class="mono">{{ t.zone }}</td>
            <td>{{ t.mode === 'axfr' ? '全量' : '增量' }}</td>
            <td><span class="tag" :class="resultClass(t)">{{ resultText(t) }}</span></td>
            <td class="mono">{{ serialText(t) }}</td>
            <td class="reason">{{ t.success ? (t.warnings[0] || '') : (t.rejections[0] || '') }}</td>
          </tr>
        </tbody>
      </table>
    `,
  };

  // ---------- 传送前后对照 ----------
  const DiffView = {
    props: ['diff'],
    template: `
      <div class="diff">
        <div v-if="!diff" class="muted">无对照数据（首次传送或区未变化时可能为空）。</div>
        <template v-else>
          <div v-if="diff.added.length === 0 && diff.removed.length === 0 && diff.modified.length === 0" class="muted">
            传送前后内容一致（{{ diff.unchanged }} 个记录集未变化）。
          </div>
          <div v-if="diff.added.length" class="diff-group">
            <h4>新增记录集（{{ diff.added.length }}）</h4>
            <div v-for="s in diff.added" :key="'a' + s.key" class="diff-set add">
              <div class="diff-set-head">+ {{ s.name }} {{ s.class }} {{ s.type }}</div>
              <div v-for="(r, i) in s.records" :key="i" class="diff-rec add mono">+ {{ r.rdata }} <span class="muted">TTL {{ r.ttl }}</span></div>
            </div>
          </div>
          <div v-if="diff.removed.length" class="diff-group">
            <h4>删除记录集（{{ diff.removed.length }}）</h4>
            <div v-for="s in diff.removed" :key="'r' + s.key" class="diff-set del">
              <div class="diff-set-head">− {{ s.name }} {{ s.class }} {{ s.type }}</div>
              <div v-for="(r, i) in s.records" :key="i" class="diff-rec del mono">− {{ r.rdata }} <span class="muted">TTL {{ r.ttl }}</span></div>
            </div>
          </div>
          <div v-if="diff.modified.length" class="diff-group">
            <h4>修改的记录集（{{ diff.modified.length }}）<span class="hint">TTL 改动属于集内修改，不是删除记录集</span></h4>
            <div v-for="s in diff.modified" :key="'m' + s.key" class="diff-set mod">
              <div class="diff-set-head">~ {{ s.name }} {{ s.class }} {{ s.type }}</div>
              <div v-for="(c, i) in s.ttlChanges" :key="'t' + i" class="diff-rec ttl mono">~ {{ c.rdata }} <span class="ttlchg">TTL {{ c.oldTtl }} → {{ c.newTtl }}</span></div>
              <div v-for="(r, i) in s.added" :key="'a' + i" class="diff-rec add mono">+ {{ r.rdata }} <span class="muted">TTL {{ r.ttl }}</span></div>
              <div v-for="(r, i) in s.removed" :key="'r' + i" class="diff-rec del mono">− {{ r.rdata }} <span class="muted">TTL {{ r.ttl }}</span></div>
            </div>
          </div>
          <div class="muted" v-if="diff.unchanged">另有 {{ diff.unchanged }} 个记录集未变化。</div>
        </template>
      </div>
    `,
  };

  // ---------- 传送详情 ----------
  const TransferDetail = {
    components: { 'diff-view': DiffView },
    props: ['detail'],
    emits: ['close'],
    data() { return { showRaw: false }; },
    computed: {
      relationText() { return RELATIONS[this.detail.serialRelation] || this.detail.serialRelation; },
    },
    methods: { fmtTime },
    template: `
      <div class="drawer">
        <div class="drawer-head">
          <h3>传送 #{{ detail.id }} · {{ detail.zone }} · {{ detail.mode === 'axfr' ? '全量' : '增量' }}</h3>
          <button class="btn" @click="$emit('close')">关闭</button>
        </div>
        <div class="kv">
          <div><span>结果</span>
            <b v-if="!detail.success" class="bad-text">失败（已拒收，旧区不变）</b>
            <b v-else-if="detail.upToDate">已是最新</b>
            <b v-else class="ok-text">成功</b>
          </div>
          <div><span>时间</span>{{ fmtTime(detail.startedAt) }}</div>
          <div><span>序列号</span><span class="mono">{{ detail.hadLocalCopy ? detail.oldSerial + ' → ' + detail.newSerial : '→ ' + detail.newSerial }}</span></div>
          <div><span>序列号关系</span>{{ relationText }}</div>
          <div v-if="detail.fellBackToFull"><span>全量回退</span>请求增量，但服务器只肯给全量，已识别并按全量处理</div>
          <div><span>收到记录</span>{{ detail.receivedCount }} 条</div>
        </div>

        <div v-if="detail.rejections && detail.rejections.length" class="block bad-block">
          <h4>拒收原因</h4>
          <ul><li v-for="(r, i) in detail.rejections" :key="i">{{ r }}</li></ul>
        </div>
        <div v-if="detail.warnings && detail.warnings.length" class="block warn-block">
          <h4>警告</h4>
          <ul><li v-for="(r, i) in detail.warnings" :key="i">{{ r }}</li></ul>
        </div>

        <div v-if="detail.segments && detail.segments.length" class="block">
          <h4>增量修订段（{{ detail.segments.length }} 段，可跨多次修订）</h4>
          <table class="tbl">
            <thead><tr><th>段</th><th>旧 SOA 序列号</th><th>新 SOA 序列号</th><th>删除项</th><th>增加项</th></tr></thead>
            <tbody>
              <tr v-for="s in detail.segments" :key="s.index">
                <td>{{ s.index }}</td><td class="mono">{{ s.oldSerial }}</td><td class="mono">{{ s.newSerial }}</td>
                <td>{{ s.deleted }}</td><td>{{ s.added }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <div v-if="detail.diff" class="block">
          <h4>传送前后对照</h4>
          <diff-view :diff="detail.diff"></diff-view>
        </div>

        <div v-if="detail.raw && detail.raw.length" class="block">
          <h4 @click="showRaw = !showRaw" class="collapsible">{{ showRaw ? '▾' : '▸' }} 实际收到的记录（{{ detail.raw.length }} 条）<span class="hint">失败时可据此检查服务器发来的数据</span></h4>
          <pre v-if="showRaw" class="raw">{{ detail.raw.join('\\n') }}</pre>
        </div>
      </div>
    `,
  };

  // ---------- 主应用 ----------
  const app = createApp({
    components: {
      'rrset-table': RrsetTable,
      'transfer-list': TransferList,
      'transfer-detail': TransferDetail,
    },
    data() {
      return {
        loading: false,
        error: null,
        notice: null,
        overview: { serverZones: [], station: [] },
        selectedZone: null,
        snapshot: null,
        snapshots: [],
        viewSerial: null,
        transfers: [],
        detail: null,
        tab: 'zone',
        tabs: [
          { key: 'zone', name: '区内容' },
          { key: 'transfers', name: '传送记录' },
          { key: 'failed', name: '失败检查' },
        ],
        behaviorDefs: [
          { key: 'wrapSerial', name: '序列号回绕', desc: 'wrap.test. 的序列号按 32 位回绕递增', tip: '切换后 wrap.test. 重置到初始版本；采集站须按 RFC 1982 比较' },
          { key: 'missingEnd', name: '缺失结束段', desc: '传送到一半就断开（无收尾 SOA 段）', tip: '采集站应拒收并保持旧区不变' },
          { key: 'duplicateRecords', name: '重复记录', desc: '传送中夹带重复记录', tip: '记录集一致性校验应拒收' },
          { key: 'refuseTransfer', name: '拒绝传送', desc: '服务器对 AXFR/IXFR 回 REFUSED', tip: '普通查询不受影响' },
          { key: 'ambiguousSerial', name: '临界序列号', desc: '下次推进版本时序列号跳变 2^31（一次性）', tip: '差值恰为 2^31 时先后关系不确定，采集站必须停止切换' },
        ],
        timer: null,
      };
    },
    computed: {
      zoneRows() {
        const station = {};
        for (const s of this.overview.station || []) station[s.name] = s;
        return (this.overview.serverZones || []).map((z) => {
          const st = station[z.name] || {};
          const hasCopy = !!st.hasCopy;
          let statusText = '未采集';
          let statusClass = '';
          if (hasCopy) {
            if (st.serial === z.serial) { statusText = '已是最新'; statusClass = 'ok'; }
            else { statusText = '可更新'; statusClass = 'warn'; }
          }
          return {
            name: z.name,
            serverSerial: z.serial,
            cur: z.cur,
            count: z.count,
            keepHistory: z.keepHistory,
            canBump: z.canBump,
            hasCopy,
            localSerial: st.serial,
            snapshotNum: st.snapshotNum || 0,
            failedNum: st.failedNum || 0,
            statusText,
            statusClass,
          };
        });
      },
      currentSerial() {
        return this.snapshot && this.viewSerial === null ? this.snapshot.serial : null;
      },
      currentSerialText() {
        const row = this.zoneRows.find((z) => z.name === this.selectedZone);
        return row && row.hasCopy ? '序列号 ' + row.localSerial : '无副本';
      },
      failedTransfers() {
        return this.transfers.filter((t) => !t.success);
      },
    },
    methods: {
      fmtTime,
      async refreshAll() {
        this.loading = true;
        try {
          const [ov, tr] = await Promise.all([api('/api/overview'), api('/api/transfers')]);
          this.overview = ov;
          this.transfers = tr;
          if (this.selectedZone) {
            await this.loadSnapshots();
            await this.loadSnapshot();
          }
          if (this.detail) {
            try { this.detail = await api('/api/transfers/' + this.detail.id); } catch (e) { this.detail = null; }
          }
        } catch (e) {
          this.error = e.message;
        } finally {
          this.loading = false;
        }
      },
      async selectZone(name) {
        this.selectedZone = name;
        this.viewSerial = null;
        this.snapshot = null;
        await this.loadSnapshots();
        await this.loadSnapshot();
      },
      async loadSnapshots() {
        try {
          this.snapshots = await api('/api/zones/' + encodeURIComponent(this.selectedZone) + '/snapshots');
        } catch (e) { this.snapshots = []; }
      },
      async loadSnapshot() {
        if (!this.selectedZone) return;
        const base = '/api/zones/' + encodeURIComponent(this.selectedZone);
        try {
          this.snapshot = this.viewSerial === null
            ? await api(base + '/snapshot')
            : await api(base + '/snapshots/' + this.viewSerial);
        } catch (e) {
          this.snapshot = null;
        }
      },
      async doTransfer(zone, mode) {
        try {
          const res = await api('/api/zones/' + encodeURIComponent(zone) + '/transfer', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ mode }),
          });
          this.detail = res;
          if (res.success) {
            this.notice = res.upToDate ? `区 ${zone} 已是最新` : `传送成功：${zone} 序列号 → ${res.newSerial}`;
            this.tab = 'zone';
          } else {
            this.error = `传送被拒收：${res.rejections[0] || '未知原因'}`;
            this.tab = 'failed';
          }
          await this.refreshAll();
          this.detail = res;
        } catch (e) {
          this.error = e.message;
        }
      },
      async doBump(zone) {
        try {
          const info = await api('/api/server/zones/' + encodeURIComponent(zone) + '/bump', { method: 'POST' });
          this.notice = `服务器区 ${zone} 已推进到序列号 ${info.serial}（版本 ${info.cur + 1}/${info.count}）`;
          await this.refreshAll();
        } catch (e) {
          this.error = e.message;
        }
      },
      async doClear(zone) {
        try {
          await api('/api/zones/' + encodeURIComponent(zone), { method: 'DELETE' });
          this.notice = `已清除 ${zone} 的本地副本`;
          await this.refreshAll();
        } catch (e) {
          this.error = e.message;
        }
      },
      async toggleBehavior(key, val) {
        const b = Object.assign({}, this.overview.behaviors, { [key]: val });
        try {
          this.overview.behaviors = await api('/api/server/behaviors', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(b),
          });
          if (key === 'wrapSerial') this.notice = '已切换序列号回绕：wrap.test. 已重置到初始版本';
          if (key === 'ambiguousSerial' && val) this.notice = '下一次「推进版本」将把序列号跳变 2^31（一次性）';
          await this.refreshAll();
        } catch (e) {
          this.error = e.message;
        }
      },
      async loadTransfer(id) {
        try {
          this.detail = await api('/api/transfers/' + id);
        } catch (e) {
          this.error = e.message;
        }
      },
      downloadZone() {
        if (!this.selectedZone) return;
        let url = '/api/zones/' + encodeURIComponent(this.selectedZone) + '/download';
        if (this.viewSerial !== null) url += '?serial=' + this.viewSerial;
        window.location = url;
      },
    },
    mounted() {
      this.refreshAll();
      this.timer = setInterval(() => this.refreshAll(), 5000);
    },
    beforeUnmount() {
      clearInterval(this.timer);
    },
  });

  app.mount('#app');
})();
