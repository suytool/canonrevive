/* CanonRevive 前端：实时预览（Canvas，与服务端 Go 算法一致） + 上传/下载/批量 */
'use strict';

const APP_VERSION = '0.1.8';

const $ = (sel) => document.querySelector(sel);

// 兼容 fnOS 反代子路径：页面可能运行在 / 或 /app-path/ 下，
// 所有接口与图片 URL 都基于当前页面目录拼接，避免绝对路径 /api 在子路径下 404。
const APP_BASE = new URL('.', document.baseURI).pathname; // 形如 '/' 或 '/app-path/'
const apiUrl = (p) => APP_BASE + String(p).replace(/^\//, '');

// ---------------------------------------------------------------------------
// 参数与预设
// ---------------------------------------------------------------------------
const SLIDERS = [
  { key: 'brightness', label: '亮度',      min: -50, max: 50 },
  { key: 'contrast',   label: '对比度',    min: -50, max: 50 },
  { key: 'saturation', label: '饱和度',    min: -100, max: 50 },
  { key: 'vibrance',   label: '鲜艳度',    min: 0,   max: 50 },
  { key: 'warmth',     label: '色温 暖→冷', min: -30, max: 30 },
  { key: 'clarity',    label: '清晰度',    min: -30, max: 30 },
  { key: 'glow',       label: '梦幻柔焦',  min: 0,   max: 100 },
  { key: 'fade',       label: '胶片褪色',  min: 0,   max: 100 },
  { key: 'grain',      label: '颗粒',      min: 0,   max: 100 },
  { key: 'vignette',   label: '暗角',      min: 0,   max: 100 },
];

const state = {
  id: null, fileName: '', width: 0, height: 0,
  origData: null, blurData: null, img: null,
  params: {}, currentPreset: null,
};

// ---------------------------------------------------------------------------
// 与服务端 Go 完全一致的像素算法
// ---------------------------------------------------------------------------
function clamp(v) {
  return v < 0 ? 0 : v > 255 ? 255 : Math.round(v);
}

function hashNoise(x, y, seed) {
  const n = Math.sin(x * 127.1 + y * 311.7 + seed * 0.5) * 43758.5453123;
  return (n - Math.floor(n)) * 2 - 1;
}

function boxBlur(data, w, h, radius) {
  const r = Math.max(1, Math.min(6, Math.round(radius)));
  const out = new Uint8ClampedArray(data.length);
  const norm = (2 * r + 1) * (2 * r + 1);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      let sr = 0, sg = 0, sb = 0;
      for (let dy = -r; dy <= r; dy++) {
        const yy = Math.min(h - 1, Math.max(0, y + dy));
        for (let dx = -r; dx <= r; dx++) {
          const xx = Math.min(w - 1, Math.max(0, x + dx));
          const i = (yy * w + xx) * 4;
          sr += data[i]; sg += data[i + 1]; sb += data[i + 2];
        }
      }
      const i = (y * w + x) * 4;
      out[i] = sr / norm; out[i + 1] = sg / norm; out[i + 2] = sb / norm; out[i + 3] = 255;
    }
  }
  return out;
}

// 柔焦：缩小 → 模糊 → 放大 → Screen 合成（与 Go 端 blurredLarge 一致）
function applyGlow(ctx, w, h, amount) {
  const sw = Math.max(1, w >> 2), sh = Math.max(1, h >> 2);
  const small = document.createElement('canvas');
  small.width = sw; small.height = sh;
  const sctx = small.getContext('2d');
  sctx.drawImage(ctx.canvas, 0, 0, sw, sh);
  // 与 Go 一致：sigma = maxEdge / 250（预览图尺度下视觉等价于原图）
  const sigma = Math.max(3, Math.min(60, Math.max(w, h) / 250));
  sctx.filter = `blur(${sigma}px)`;
  sctx.drawImage(small, 0, 0, sw, sh);
  sctx.filter = 'none';

  const glow = document.createElement('canvas');
  glow.width = w; glow.height = h;
  const gctx = glow.getContext('2d');
  gctx.imageSmoothingEnabled = true;
  gctx.imageSmoothingQuality = 'high';
  gctx.drawImage(small, 0, 0, w, h);

  ctx.save();
  ctx.globalCompositeOperation = 'screen';
  ctx.globalAlpha = amount * 0.9;
  ctx.drawImage(glow, 0, 0);
  ctx.restore();
}

function render() {
  if (!state.origData) return;
  try {
    renderInner();
  } catch (e) {
    // 渲染失败时把错误显示出来，便于定位
    setStatus('预览渲染失败：' + (e && e.message ? e.message : e), true);
    console.error('render error:', e);
  }
}

function renderInner() {
  const ctx = $('#previewCanvas').getContext('2d');
  // 用 Canvas 实际尺寸（预览图尺寸），必须与 origData 一致
  const w = ctx.canvas.width, h = ctx.canvas.height;
  const p = state.params;

  const bright = p.brightness * 2.55;
  const cf = (100 + p.contrast) / 100;
  const sf = (100 + p.saturation) / 100;
  const vf0 = p.vibrance / 100 * 0.6;
  const warm = p.warmth * 0.0009;
  const ck = p.clarity / 100 * 0.9;
  const f = p.fade / 100;
  const gAmp = p.grain / 100 * 22;
  const vg = p.vignette / 100;
  const invMaxR2 = 1 / ((w * w + h * h) / 4);
  const seed = 0x9E3779B9;

  const src = state.origData.data;
  const blurD = state.blurData;
  const out = new ImageData(new Uint8ClampedArray(src.length), w, h);
  const d = out.data;

  for (let i = 0; i < d.length; i += 4) {
    let r = src[i], g = src[i + 1], b = src[i + 2];
    // 1. 清晰度 unsharp
    if (ck !== 0) {
      r = clamp(r + (r - blurD[i]) * ck);
      g = clamp(g + (g - blurD[i + 1]) * ck);
      b = clamp(b + (b - blurD[i + 2]) * ck);
    }
    // 2. base：对比 → 亮度 → 饱和/鲜艳 → 色温（YCbCr，BT.601）
    //    与 Go 标准库 color.RGBToYCbCr / YCbCrToRGB 逐位一致：
    //    - RGB→YCbCr 用 JFIF 直接展开式（257<<15 舍入 + 快速 clamp）
    //    - YCbCr→RGB 用 y*0x10101 定点
    const y = (19595 * r + 38470 * g + 7471 * b + (1 << 15)) >> 16;
    let cb = -11056 * r - 21712 * g + 32768 * b + (257 << 15);
    let cr = 32768 * r - 27440 * g - 5328 * b + (257 << 15);
    if (((cb >>> 0) & 0xff000000) === 0) cb >>= 16; else cb = ~(cb >> 31);
    if (((cr >>> 0) & 0xff000000) === 0) cr >>= 16; else cr = ~(cr >> 31);
    cb &= 0xff; cr &= 0xff;
    const yf = clamp((y - 128) * cf + 128 + bright);
    const sat = Math.min(1, Math.sqrt((cb - 128) ** 2 + (cr - 128) ** 2) / 127);
    const sMul = sf * (1 + vf0 * (1 - sat));
    cb = clamp(128 + (cb - 128) * sMul);
    cr = clamp(128 + (cr - 128) * sMul);
    const yy = yf | 0, ccb = cb - 128, ccr = cr - 128;
    let nr = ((yy * 0x10101 + 91881 * ccr) >> 16);
    let ng = ((yy * 0x10101 - 22554 * ccb - 46802 * ccr) >> 16);
    let nb = ((yy * 0x10101 + 116130 * ccb) >> 16);
    if (warm !== 0) { nr = nr * (1 + warm); nb = nb * (1 - warm); }
    d[i] = clamp(nr); d[i + 1] = clamp(ng); d[i + 2] = clamp(nb); d[i + 3] = 255;
  }
  ctx.putImageData(out, 0, 0);

  // 3. 梦幻柔焦（Screen 合成）
  if (p.glow > 0) applyGlow(ctx, w, h, p.glow / 100);

  // 4+5+6. fade / grain / vignette
  if (f > 0 || gAmp > 0 || vg > 0) {
    const id2 = ctx.getImageData(0, 0, w, h);
    const d2 = id2.data;
    for (let i = 0; i < d2.length; i += 4) {
      const x = (i / 4) % w, y = Math.floor(i / 4 / w);
      let r = d2[i], g = d2[i + 1], b = d2[i + 2];
      if (f > 0) {
        r = r * (1 - f) + 26 * f;
        g = g * (1 - f) + 26 * f;
        b = b * (1 - f) + 26 * f;
      }
      if (gAmp > 0) {
        const gv = hashNoise(x, y, seed) * gAmp;
        r += gv; g += gv; b += gv;
      }
      if (vg > 0) {
        const dx = x - (w - 1) / 2, dy = y - (h - 1) / 2;
        const d2v = (dx * dx + dy * dy) * invMaxR2;
        const vf = 1 - vg * 0.32 * d2v * d2v;
        r *= vf; g *= vf; b *= vf;
      }
      d2[i] = clamp(r); d2[i + 1] = clamp(g); d2[i + 2] = clamp(b);
    }
    ctx.putImageData(id2, 0, 0);
  }
}

// ---------------------------------------------------------------------------
// 渲染调度（滑杆拖动防抖）
// ---------------------------------------------------------------------------
let renderTimer = null;
function scheduleRender() {
  if (renderTimer) clearTimeout(renderTimer);
  renderTimer = setTimeout(render, 60);
}

// ---------------------------------------------------------------------------
// 预设
// ---------------------------------------------------------------------------
let presets = [];
async function loadPresets() {
  const res = await fetch(apiUrl('/api/presets'));
  presets = await res.json();
  const grid = $('#presetGrid');
  grid.innerHTML = '';
  for (const pr of presets) {
    const card = document.createElement('button');
    card.className = 'preset-card';
    card.dataset.key = pr.key;
    card.innerHTML = `<strong>${pr.name}</strong><span>${pr.desc}</span>`;
    card.onclick = () => applyPreset(pr.key);
    grid.appendChild(card);
  }
  // 同步填充批量处理的效果下拉框
  const sel = $('#batchPreset');
  if (sel) {
    sel.innerHTML = presets.map((p) => `<option value="${p.key}">${p.name}</option>`).join('')
      + '<option value="custom" disabled>当前编辑参数（先上传照片微调后可用）</option>';
    sel.value = 'lcd';
  }
}

function applyPreset(key) {
  const pr = presets.find((x) => x.key === key);
  if (!pr) return;
  state.params = { ...pr.params };
  state.currentPreset = key;
  syncSliders();
  highlightPreset();
  scheduleRender();
}

function highlightPreset() {
  document.querySelectorAll('.preset-card').forEach((c) => {
    c.classList.toggle('active', c.dataset.key === state.currentPreset);
  });
}

// ---------------------------------------------------------------------------
// 滑杆
// ---------------------------------------------------------------------------
function buildSliders() {
  const box = $('#sliders');
  box.innerHTML = '';
  for (const s of SLIDERS) {
    const row = document.createElement('div');
    row.className = 'slider-row';
    row.innerHTML = `
      <label>${s.label}</label>
      <input type="range" min="${s.min}" max="${s.max}" step="1" data-key="${s.key}">
      <span class="val" data-val="${s.key}">0</span>`;
    const input = row.querySelector('input');
    input.addEventListener('input', () => {
      state.params[s.key] = Number(input.value);
      state.currentPreset = null;
      highlightPreset();
      row.querySelector('[data-val]').textContent = input.value;
      scheduleRender();
    });
    box.appendChild(row);
  }
}

function syncSliders() {
  document.querySelectorAll('#sliders input').forEach((input) => {
    const k = input.dataset.key;
    input.value = state.params[k] ?? 0;
    input.closest('.slider-row').querySelector('.val').textContent = input.value;
  });
}

// ---------------------------------------------------------------------------
// 上传
// ---------------------------------------------------------------------------
async function uploadFile(file) {
  const fd = new FormData();
  fd.append('file', file);
  setStatus('正在上传并解析照片…');
  const res = await fetch(apiUrl('/api/upload'), { method: 'POST', body: fd });
  if (!res.ok) {
    const msg = await res.text();
    setStatus('上传失败：' + msg, true);
    return;
  }
  const info = await res.json();
  state.id = info.id;
  state.fileName = info.name;

  const img = new Image();
  img.onload = () => {
    const orig = $('#origCanvas');
    orig.width = img.naturalWidth; orig.height = img.naturalHeight;
    orig.getContext('2d').drawImage(img, 0, 0);

    const prev = $('#previewCanvas');
    prev.width = img.naturalWidth; prev.height = img.naturalHeight;
    state.width = img.naturalWidth;
    state.height = img.naturalHeight;

    const octx = orig.getContext('2d');
    state.origData = octx.getImageData(0, 0, img.naturalWidth, img.naturalHeight);
    state.blurData = boxBlur(state.origData.data, img.naturalWidth, img.naturalHeight,
      Math.max(img.naturalWidth, img.naturalHeight) / 1000);

    $('#uploadZone').hidden = true;
    $('#editor').hidden = false;
    $('#fileMeta').textContent = `${info.name} · ${info.width} × ${info.height}`;

    applyPreset('lcd'); // 默认“相机屏显还原”
    // 上传照片后，批量处理可选用当前微调参数
    const customOpt = $('#batchPreset option[value="custom"]');
    if (customOpt) customOpt.disabled = false;
    layoutMobilePreview();
    setStatus('照片已就绪，调整参数后下载');
  };
  img.onerror = () => setStatus('预览图加载失败', true);
  img.src = apiUrl(info.previewUrl);
}

// ---------------------------------------------------------------------------
// 下载
// ---------------------------------------------------------------------------
async function download() {
  if (!state.id) return;
  setStatus('正在按原图分辨率渲染…');
  const btn = $('#downloadBtn');
  btn.disabled = true;
  try {
    const res = await fetch(apiUrl('/api/render'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: state.id, params: state.params, file: outFileName() }),
    });
    if (!res.ok) throw new Error(await res.text());
    const blob = await res.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = outFileName();
    a.click();
    URL.revokeObjectURL(a.href);
    setStatus('已导出：' + outFileName());
  } catch (e) {
    setStatus('渲染失败：' + e.message, true);
  } finally {
    btn.disabled = false;
  }
}

function outFileName() {
  const base = state.fileName.replace(/\.(jpe?g|png)$/i, '');
  return base + '_屏显还原.jpg';
}

// ---------------------------------------------------------------------------
// 批量
// ---------------------------------------------------------------------------
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// 批量后台任务的轮询句柄（任务在服务端独立运行，关闭界面不影响）。
let batchTimer = null;

function batchSetRunningUI(isRunning) {
  $('#batchBtn').hidden = isRunning;
  $('#batchStopBtn').hidden = !isRunning;
}

function stopBatchPolling() {
  if (batchTimer) { clearTimeout(batchTimer); batchTimer = null; }
}

// 根据状态快照渲染进度条与结果。
function renderBatchState(st) {
  const progress = $('#batchProgress');
  const fill = $('#progressFill');
  const ptext = $('#progressText');
  const result = $('#batchResult');

  if (st.state === 'idle' || st.total === 0) {
    progress.hidden = true;
    return;
  }
  progress.hidden = false;
  if (st.inDir) $('#batchInPath').textContent = st.inDir;
  if (st.outDir) $('#batchOutPath').textContent = st.outDir;
  const pct = st.total ? Math.round((st.done / st.total) * 100) : 0;

  if (st.state === 'running') {
    fill.style.width = pct + '%';
    ptext.innerHTML =
      `正在处理 <span class="pct">${pct}%</span>（${st.done}/${st.total}）· 成功 ${st.ok} 跳过 ${st.skip}` +
      `<br><span class="cur">${escapeHtml(st.current || '')}</span>`;
    batchSetRunningUI(true);
  } else if (st.state === 'finished') {
    fill.style.width = '100%';
    ptext.innerHTML = '<span class="pct">100%</span> 处理完成';
    batchSetRunningUI(false);
    let msg = `完成：成功 ${st.ok} 张，跳过 ${st.skip} 张`;
    if (st.skip > 0) msg += '（' + (st.skipped || []).join('、') + '）';
    if (st.warning) msg += '。提示：' + st.warning;
    result.textContent = msg;
    stopBatchPolling();
  } else if (st.state === 'canceled') {
    fill.style.width = pct + '%';
    ptext.textContent = `已停止：处理到第 ${st.done}/${st.total} 张`;
    batchSetRunningUI(false);
    result.textContent = `已停止：已成功 ${st.ok} 张，跳过 ${st.skip} 张`;
    stopBatchPolling();
  }
}

// 轮询服务端任务状态（600ms 一次，网络抖动自动重试）。
function startBatchPolling() {
  stopBatchPolling();
  const tick = async () => {
    try {
      const res = await fetch(apiUrl('/api/batch/status'));
      const st = await res.json();
      renderBatchState(st);
      if (st.state === 'running') batchTimer = setTimeout(tick, 600);
    } catch {
      batchTimer = setTimeout(tick, 1500);
    }
  };
  batchTimer = setTimeout(tick, 200);
}

// 点击“开始批量处理”：启动（或复用）服务端后台任务，随后轮询。
async function batch() {
  const result = $('#batchResult');
  result.textContent = '';
  const sel = $('#batchPreset');
  const choice = sel ? sel.value : 'lcd';
  let params;
  if (choice === 'custom') {
    params = state.params;
  } else {
    const pr = presets.find((x) => x.key === choice);
    params = pr ? pr.params : state.params;
  }
  try {
    const res = await fetch(apiUrl('/api/batch'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ params }),
    });
    const txt = await res.text();
    let st;
    try { st = JSON.parse(txt); } catch { throw new Error(txt); }
    if (!res.ok) throw new Error(st.error || ('HTTP ' + res.status));
    if (st.total === 0) {
      $('#batchProgress').hidden = false;
      $('#progressFill').style.width = '100%';
      $('#progressText').textContent = '输入目录中没有可处理的照片';
      batchSetRunningUI(false);
      return;
    }
    renderBatchState(st);
    if (st.state === 'running') startBatchPolling();
  } catch (e) {
    result.textContent = '批量处理失败：' + e.message;
  }
}

// 点击“停止批量处理”：请求服务端停止后台任务。
async function stopBatch() {
  try {
    await fetch(apiUrl('/api/batch/stop'), { method: 'POST' });
    const res = await fetch(apiUrl('/api/batch/status'));
    renderBatchState(await res.json());
  } catch (e) {
    $('#batchResult').textContent = '停止失败：' + e.message;
  }
}

// 页面加载（重新打开界面）时恢复正在进行或已完成的任务。
async function restoreBatch() {
  try {
    const res = await fetch(apiUrl('/api/batch/status'));
    const st = await res.json();
    if (st.state === 'running') {
      renderBatchState(st);
      startBatchPolling();
    } else if (st.state === 'finished' || st.state === 'canceled') {
      renderBatchState(st);
    }
  } catch { /* 无任务或网络暂不可达，忽略 */ }
}

// ---------------------------------------------------------------------------
// 通用
// ---------------------------------------------------------------------------
function setStatus(msg, isError = false) {
  const el = $('#status');
  const ver = el.querySelector('.ver');
  el.textContent = msg;
  if (ver) el.appendChild(ver); // 状态文本替换后把版本徽标挪回末尾
  el.classList.toggle('error', isError);
}

// 手机端：预览区 fixed 悬浮顶部，用 padding 撑开内容区避免遮挡
function layoutMobilePreview() {
  const editor = $('#editor');
  const stage = $('.compare-stage');
  if (!editor || !stage) return;
  if (window.innerWidth > 640 || editor.hidden) {
    editor.style.paddingTop = '';
    return;
  }
  editor.style.paddingTop = (stage.offsetHeight + 12) + 'px';
}

function init() {
  loadPresets();
  buildSliders();
  $('#resetBtn').onclick = () => { applyPreset('lcd'); setStatus('已重置为“相机屏显还原”'); };
  $('#downloadBtn').onclick = download;
  $('#batchBtn').onclick = batch;
  $('#batchStopBtn').onclick = stopBatch;
  $('#pickBtn').onclick = () => $('#fileInput').click();
  $('#fileInput').onchange = (e) => {
    if (e.target.files[0]) uploadFile(e.target.files[0]);
  };
  // 拖拽上传
  const zone = $('#uploadZone');
  ['dragover', 'dragenter'].forEach((ev) =>
    zone.addEventListener(ev, (e) => { e.preventDefault(); zone.classList.add('drag'); }));
  ['dragleave', 'drop'].forEach((ev) =>
    zone.addEventListener(ev, (e) => { e.preventDefault(); zone.classList.remove('drag'); }));
  zone.addEventListener('drop', (e) => {
    const f = e.dataTransfer.files && e.dataTransfer.files[0];
    if (f) uploadFile(f);
  });
  // 批量路径提示
  fetch(apiUrl('/api/info')).then((r) => r.json()).then((info) => {
    $('#batchInPath').textContent = info.inDir;
    $('#batchOutPath').textContent = info.outDir;
  }).catch(() => {});
  // 快捷键：R 重置，S 下载
  document.addEventListener('keydown', (e) => {
    if (e.key === 'r' || e.key === 'R') { $('#resetBtn').click(); }
    if (e.key === 's' || e.key === 'S') { $('#downloadBtn').click(); }
  });
  // 版本徽标（用于确认浏览器加载的是最新前端）
  const verEl = $('#versionTag');
  if (verEl) verEl.textContent = 'v' + APP_VERSION;
  // 窗口尺寸变化时重新计算手机端预览占位
  window.addEventListener('resize', () => layoutMobilePreview());
  // 支持作者弹窗
  const modal = $('#supportModal');
  $('#supportBtn').onclick = () => { modal.hidden = false; };
  const closeModal = () => { modal.hidden = true; };
  $('#supportClose').onclick = closeModal;
  modal.addEventListener('click', (e) => { if (e.target === modal) closeModal(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });
  // 恢复服务端可能仍在运行（或已完成）的后台批量任务
  restoreBatch();
}

init();
