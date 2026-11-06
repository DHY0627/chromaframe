/*
 * Chromaframe UI controller.
 * Talks to the local Go server, drives the live Chromium preview and mirrors
 * the headless render progress while a render is running.
 */
(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };
  var els = {
    htmlPath: $('htmlPath'), browseHtml: $('browseHtml'), htmlHint: $('htmlHint'),
    width: $('width'), height: $('height'), ratioRow: $('ratioRow'),
    duration: $('duration'), autoDuration: $('autoDuration'), fps: $('fps'),
    output: $('output'), browseOutput: $('browseOutput'), revealOutput: $('revealOutput'),
    audio: $('audio'), browseAudio: $('browseAudio'),
    copyPath: $('copyPath'),
    ffmpegCard: $('ffmpegCard'), ffmpegDot: $('ffmpegDot'), ffmpegCardText: $('ffmpegCardText'),
    ffmpegProgress: $('ffmpegProgress'), ffmpegProgressFill: $('ffmpegProgressFill'), ffmpegFoot: $('ffmpegFoot'),
    renderBtn: $('renderBtn'), renderBtnText: $('renderBtnText'), cancelBtn: $('cancelBtn'), actionNote: $('actionNote'),
    stage: $('stage'), emptyState: $('emptyState'), emptyPick: $('emptyPick'),
    canvasFrame: $('canvasFrame'), preview: $('preview'), renderFrame: $('renderFrame'),
    renderScan: $('renderScan'), frameBadge: $('frameBadge'),
    stageBadge: $('stageBadge'), stageName: $('stageName'), liveDot: $('liveDot'),
    resolutionText: $('resolutionText'), durationText: $('durationText'), fpsText: $('fpsText'),
    playBtn: $('playBtn'), playIcon: $('playIcon'), curTime: $('curTime'), totalTime: $('totalTime'),
    scrub: $('scrub'), scrubFill: $('scrubFill'), frameCount: $('frameCount'),
    stageText: $('stageText'), progressText: $('progressText'), statusDot: $('statusDot'),
    openOutputBtn: $('openOutputBtn'), toasts: $('toasts'),
    chromeChip: $('chromeChip'), chromeText: $('chromeText'),
    ffmpegChip: $('ffmpegChip'), ffmpegText: $('ffmpegText'), versionChip: $('versionChip')
  };

  var SETTINGS_KEY = 'chromaframe.settings.v1';
  var RENDER_POLL_MS = 360;
  var IDLE_POLL_MS = 1200;

  var S = {
    token: '',
    chromium: { available: false },
    ffmpeg: { available: false },
    render: {},
    hasHtml: false,
    autoLoadAttempted: false,
    htmlPath: '',
    htmlName: '',
    defaultOutput: '',
    outputAuto: true,
    width: 1920,
    height: 1080,
    duration: 10,
    fps: 30,
    time: 0,
    playing: false,
    iframeReady: false,
    estimatedDuration: 0,
    renderActive: false,
    renderSnapshot: null,
    lastFrameRev: -1,
    inflightSeeks: 0,
    seekSeq: 0,
    playRaf: 0,
    playStart: 0,
    playBase: 0,
    lastPosted: -1,
    lastPostAt: 0,
    pollTimer: 0,
    pollFailures: 0,
    busy: false,
    estimateSeq: 0,
    estimateWaiters: {},
    scrubbing: false
  };

  var TOTAL_FRAMES = function () {
    return Math.max(1, Math.round(S.duration * S.fps));
  };

  // ---------------------------------------------------------------- API ----

  function api(path, body) {
    var opt = { credentials: 'same-origin', headers: {} };
    if (S.token) opt.headers['X-CF-Token'] = S.token;
    if (body !== undefined) {
      opt.method = 'POST';
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    return fetch(path, opt).then(function (res) {
      return res.text().then(function (text) {
        var data = null;
        try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
        if (!res.ok) {
          var msg = data && data.error ? data.error : (text || ('HTTP ' + res.status));
          var err = new Error(msg);
          err.status = res.status;
          throw err;
        }
        return data;
      });
    });
  }

  function toast(message, type, ttl) {
    var el = document.createElement('div');
    el.className = 'toast ' + (type || 'info');
    el.textContent = message;
    els.toasts.appendChild(el);
    var life = ttl || (type === 'error' ? 7000 : 3800);
    setTimeout(function () {
      el.classList.add('out');
      setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 220);
    }, life);
    while (els.toasts.children.length > 4) {
      els.toasts.removeChild(els.toasts.firstChild);
    }
  }

  // ------------------------------------------------------------ helpers ----

  function clamp(v, lo, hi) { return Math.min(hi, Math.max(lo, v)); }

  function pad2(n) { return n < 10 ? '0' + n : String(n); }

  function formatTime(seconds) {
    var s = Math.max(0, Number(seconds) || 0);
    var m = Math.floor(s / 60);
    var rest = s - m * 60;
    if (rest > 59.999) { rest = 0; m += 1; }
    return pad2(m) + ':' + (rest < 10 ? '0' : '') + rest.toFixed(2);
  }

  function shortVersion(v) {
    if (!v) return '';
    var m = /(\d+\.\d+(?:\.\d+)?)/.exec(String(v));
    return m ? m[1] : String(v).slice(0, 18);
  }

  function makeEven(n) {
    n = Math.round(n);
    if (n % 2 !== 0) n += 1;
    return n;
  }

  function readInt(el, fallback) {
    var v = parseInt(el.value, 10);
    return isFinite(v) ? v : fallback;
  }

  function setNote(message, kind) {
    els.actionNote.textContent = message || '';
    els.actionNote.className = 'action-note' + (kind ? ' ' + kind : '');
  }

  function setStatus(text, kind) {
    els.stageText.textContent = text;
    els.statusDot.className = 'status-dot' + (kind ? ' ' + kind : '');
  }

  // ------------------------------------------------------------ settings ---

  function saveSettings() {
    try {
      localStorage.setItem(SETTINGS_KEY, JSON.stringify({
        htmlPath: els.htmlPath.value.trim(),
        output: els.output.value.trim(),
        audio: els.audio.value.trim(),
        outputAuto: S.outputAuto,
        width: S.width, height: S.height,
        duration: S.duration, fps: S.fps
      }));
    } catch (e) { /* storage may be disabled */ }
  }

  function restoreSettings() {
    var raw = null;
    try { raw = localStorage.getItem(SETTINGS_KEY); } catch (e) {}
    if (!raw) return null;
    try {
      var v = JSON.parse(raw);
      if (!v || typeof v !== 'object') return null;
      if (v.width) els.width.value = String(v.width);
      if (v.height) els.height.value = String(v.height);
      if (v.duration) els.duration.value = String(v.duration);
      if (v.fps) els.fps.value = String(v.fps);
      if (typeof v.output === 'string') els.output.value = v.output;
      if (typeof v.audio === 'string') els.audio.value = v.audio;
      if (typeof v.outputAuto === 'boolean') S.outputAuto = v.outputAuto;
      if (typeof v.htmlPath === 'string') els.htmlPath.value = v.htmlPath;
      return v;
    } catch (e) { return null; }
  }

  // -------------------------------------------------------------- chips ----

  function renderChips() {
    var c = S.chromium || {};
    els.chromeChip.className = 'chip ' + (c.available ? 'ok' : 'bad');
    els.chromeText.textContent = c.available ? (c.name || 'Chromium') : '未找到';
    els.chromeChip.title = c.available ? (c.path + (c.version ? '\n' + c.version : '')) : (c.error || '未找到 Chrome / Edge');

    var f = S.ffmpeg || {};
    if (f.available) {
      els.ffmpegChip.className = 'chip ok';
      els.ffmpegText.textContent = shortVersion(f.version) || '就绪';
    } else if (f.downloading) {
      els.ffmpegChip.className = 'chip busy';
      els.ffmpegText.textContent = '下载中 ' + Math.round(f.progress >= 0 ? f.progress * 100 : 0) + '%';
    } else {
      els.ffmpegChip.className = 'chip bad';
      els.ffmpegText.textContent = '未找到';
    }
    els.ffmpegChip.title = f.available ? (f.path || '') : (f.error || '点击自动下载 FFmpeg');

    renderFFmpegCard();
  }

  function renderFFmpegCard() {
    var f = S.ffmpeg || {};
    els.ffmpegCard.className = 'ffmpeg-card';
    els.ffmpegProgress.hidden = true;
    els.ffmpegFoot.textContent = '';

    if (f.available) {
      els.ffmpegCard.classList.add('ok');
      els.ffmpegCardText.textContent = 'FFmpeg 就绪' + (f.source ? ' · ' + f.source : '');
      els.ffmpegFoot.textContent = f.path || '';
      return;
    }
    if (f.downloading) {
      els.ffmpegCard.classList.add('busy');
      els.ffmpegCardText.textContent = '正在自动下载 FFmpeg…';
      els.ffmpegProgress.hidden = false;
      var pct = f.progress >= 0 ? clamp(f.progress, 0, 1) : 0;
      els.ffmpegProgressFill.style.width = (pct * 100).toFixed(1) + '%';
      if (f.total > 0) {
        els.ffmpegFoot.textContent = humanBytes(f.bytes || 0) + ' / ' + humanBytes(f.total);
      } else {
        els.ffmpegFoot.textContent = humanBytes(f.bytes || 0) + ' 已下载';
      }
      return;
    }
    els.ffmpegCard.classList.add('bad');
    els.ffmpegCardText.textContent = 'FFmpeg 未就绪';
    els.ffmpegFoot.textContent = f.error ? f.error : '导出 MP4 需要 FFmpeg，点击自动下载。';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'ffmpeg-action';
    btn.textContent = f.error && /仅支持|manual|手动/i.test(f.error) ? '查看说明' : '下载 FFmpeg';
    btn.addEventListener('click', function () { downloadFFmpeg(true); });
    els.ffmpegFoot.appendChild(btn);
  }

  function humanBytes(n) {
    n = Number(n) || 0;
    if (n >= 1024 * 1024 * 1024) return (n / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
    if (n >= 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
    if (n >= 1024) return (n / 1024).toFixed(1) + ' KB';
    return n + ' B';
  }

  // ---------------------------------------------------------- state poll ---

  function schedulePoll(delay) {
    clearTimeout(S.pollTimer);
    S.pollTimer = setTimeout(pollState, delay);
  }

  function pollState() {
    api('/api/state').then(function (st) {
      S.pollFailures = 0;
      if (!st) return;
      if (st.token) S.token = st.token;
      if (st.version) els.versionChip.textContent = 'v' + st.version;
      S.chromium = st.chromium || { available: false };
      S.ffmpeg = st.ffmpeg || { available: false };
      if (st.defaultOutput) S.defaultOutput = st.defaultOutput;
      if (st.htmlName && !S.htmlPath) S.htmlName = st.htmlName;
      if (!S.hasHtml && !S.autoLoadAttempted && st.hasHtml && st.htmlPath) {
        S.autoLoadAttempted = true;
        applyHTMLPath(st.htmlPath, { silent: true });
      }

      renderChips();
      handleRenderSnapshot(st.render || {});
      updateRenderButton();

      var active = S.renderActive || (S.ffmpeg && S.ffmpeg.downloading);
      schedulePoll(active ? RENDER_POLL_MS : IDLE_POLL_MS);
    }).catch(function (err) {
      S.pollFailures += 1;
      if (S.pollFailures === 1) {
        setStatus('无法连接本地服务，正在重试…', 'bad');
      }
      if (S.pollFailures > 20) {
        schedulePoll(4000);
      } else {
        schedulePoll(1000);
      }
    });
  }

  // ------------------------------------------------------------- preview ---

  function loadPreview(url) {
    S.iframeReady = false;
    S.playing = false;
    cancelAnimationFrame(S.playRaf);
    els.playBtn.classList.remove('playing');
    els.playIcon.textContent = '▶';
    els.preview.src = url;
    els.emptyState.hidden = true;
    els.canvasFrame.hidden = false;
    els.renderFrame.hidden = true;
    els.renderFrame.removeAttribute('src');
    els.renderScan.hidden = true;
    els.frameBadge.hidden = true;
    els.stageBadge.textContent = 'LOADING';
    els.liveDot.className = 'live-dot on';
    S.time = 0;
    S.lastFrameRev = -1;
    updateTimeline();
    fitCanvas();
    setStatus('正在载入页面与远程资源…', 'busy');
    setNote('页面内的 CDN 资源会等待加载完成。');
    updatePlayState();
  }

  function applyHTMLPath(path, opts) {
    opts = opts || {};
    path = String(path || '').trim();
    if (!path) return Promise.resolve(false);
    els.htmlPath.classList.remove('invalid');
    els.htmlHint.className = 'hint';
    els.htmlHint.textContent = '正在检查文件…';
    return api('/api/html', { path: path }).then(function (resp) {
      S.hasHtml = true;
      S.htmlPath = resp.path;
      S.htmlName = resp.name;
      S.defaultOutput = resp.defaultOutput || '';
      els.htmlPath.value = resp.path;
      els.stageName.textContent = resp.name;
      if (S.outputAuto || !els.output.value.trim()) {
        els.output.value = resp.defaultOutput || '';
        S.outputAuto = true;
      }
      els.htmlHint.className = 'hint good';
      els.htmlHint.textContent = '已载入 · ' + resp.name;
      loadPreview(resp.url);
      updateRenderButton();
      saveSettings();
      if (!opts.silent) toast('已载入 ' + resp.name, 'success', 2200);
      return true;
    }).catch(function (err) {
      S.hasHtml = false;
      els.htmlPath.classList.add('invalid');
      els.htmlHint.className = 'hint error';
      els.htmlHint.textContent = err.message;
      updateRenderButton();
      if (!opts.silent) toast(err.message, 'error');
      return false;
    });
  }

  function fitCanvas() {
    if (!S.hasHtml) return;
    var rect = els.stage.getBoundingClientRect();
    var pad = 30;
    var availW = Math.max(60, rect.width - pad * 2);
    var availH = Math.max(60, rect.height - pad * 2);
    var scale = Math.min(availW / S.width, availH / S.height);
    scale = Math.min(scale, 1);
    els.canvasFrame.style.width = S.width + 'px';
    els.canvasFrame.style.height = S.height + 'px';
    els.canvasFrame.style.transform = 'translate(-50%, -50%) scale(' + scale + ')';
  }

  // -------------------------------------------------------------- timeline --

  function updateTimeline(override) {
    var t = override && typeof override.time === 'number' ? override.time : S.time;
    var total = S.duration;
    var frames = TOTAL_FRAMES();
    var shownFrame = override && typeof override.frame === 'number'
      ? override.frame
      : Math.min(frames, Math.max(0, Math.round(t * S.fps)));
    var shownTotal = override && override.totalFrames ? override.totalFrames : frames;

    els.curTime.textContent = formatTime(t);
    els.totalTime.textContent = formatTime(total);
    els.frameCount.textContent = shownFrame + ' / ' + shownTotal;

    var pct = total > 0 ? clamp(t / total, 0, 1) : 0;
    els.scrub.value = String(Math.round(pct * 10000));
    els.scrub.style.setProperty('--fill', (pct * 100).toFixed(2) + '%');
    els.scrubFill.style.width = (pct * 100).toFixed(2) + '%';
  }

  function updateMeta() {
    els.resolutionText.textContent = S.width + ' × ' + S.height;
    els.durationText.textContent = S.duration.toFixed(1) + 's';
    els.fpsText.textContent = S.fps + 'fps';
    els.width.value = String(S.width);
    els.height.value = String(S.height);
    els.duration.value = String(S.duration);
    els.fps.value = String(S.fps);
    updateTimeline();
    updateRatioButtons();
    fitCanvas();
  }

  function updateRatioButtons() {
    var buttons = els.ratioRow.querySelectorAll('.ratio');
    var current = S.width / S.height;
    for (var i = 0; i < buttons.length; i++) {
      var b = buttons[i];
      var r = Number(b.getAttribute('data-w')) / Number(b.getAttribute('data-h'));
      b.classList.toggle('active', Math.abs(r - current) < 0.002);
    }
  }

  function updatePlayState() {
    var canPlay = S.hasHtml && !S.renderActive;
    els.playBtn.disabled = !canPlay;
    els.scrub.disabled = !canPlay || S.playing;
  }

  function setTime(seconds, seek) {
    S.time = clamp(Number(seconds) || 0, 0, S.duration);
    updateTimeline();
    if (seek) previewSeek(S.time);
  }

  function previewSeek(t) {
    if (!S.iframeReady) return;
    if (S.inflightSeeks >= 2) return;
    S.inflightSeeks += 1;
    var id = ++S.seekSeq;
    postToPreview({ type: 'set-time', time: t, id: id });
    setTimeout(function () {
      if (S.inflightSeeks > 0) S.inflightSeeks -= 1;
    }, 1500);
  }

  function postToPreview(msg) {
    if (!els.preview.contentWindow) return;
    msg.source = 'chromaframe-host';
    try { els.preview.contentWindow.postMessage(msg, '*'); } catch (e) {}
  }

  function startPlay() {
    if (!S.hasHtml || S.renderActive) return;
    if (S.time >= S.duration - 0.002) S.time = 0;
    S.playing = true;
    S.playBase = S.time;
    S.playStart = performance.now();
    S.lastPosted = -1;
    els.playBtn.classList.add('playing');
    els.playIcon.textContent = '❚❚';
    els.scrub.disabled = true;
    S.playRaf = requestAnimationFrame(playTick);
  }

  function pausePlay() {
    S.playing = false;
    cancelAnimationFrame(S.playRaf);
    els.playBtn.classList.remove('playing');
    els.playIcon.textContent = '▶';
    els.scrub.disabled = !S.hasHtml || S.renderActive;
    if (S.hasHtml) previewSeek(S.time);
  }

  function togglePlay() {
    if (S.playing) pausePlay(); else startPlay();
  }

  function playTick(now) {
    if (!S.playing) return;
    var t = S.playBase + (now - S.playStart) / 1000;
    if (t >= S.duration) {
      setTime(S.duration, false);
      pausePlay();
      return;
    }
    S.time = t;
    updateTimeline();
    var minGap = 1000 / Math.max(1, S.fps) * 0.85;
    if (t - S.lastPosted >= minGap / 1000 && now - S.lastPostAt >= minGap) {
      S.lastPosted = t;
      S.lastPostAt = now;
      previewSeek(t);
    }
    S.playRaf = requestAnimationFrame(playTick);
  }

  // --------------------------------------------------------------- render ---

  function updateRenderButton() {
    var chromiumOK = !!(S.chromium && S.chromium.available);
    var enabled = S.hasHtml && chromiumOK && !S.renderActive && !S.busy;
    els.renderBtn.disabled = !enabled;
    if (!S.renderActive) {
      els.renderBtnText.textContent = '开始渲染';
      els.cancelBtn.hidden = true;
    }
    if (S.hasHtml && !chromiumOK) {
      setNote(S.chromium && S.chromium.error ? S.chromium.error : '未找到 Chromium，无法渲染。', 'error');
    } else if (!S.hasHtml) {
      setNote('选择 HTML 后即可开始渲染。');
    } else if (!S.renderActive) {
      setNote('渲染期间时间轴会展示实时进度。');
    }
  }

  function setControlsBusy(busy) {
    S.busy = busy;
    var controls = [els.htmlPath, els.browseHtml, els.emptyPick, els.width, els.height,
      els.duration, els.autoDuration, els.fps, els.output, els.browseOutput,
      els.audio, els.browseAudio];
    for (var i = 0; i < controls.length; i++) controls[i].disabled = busy;
    var ratios = els.ratioRow.querySelectorAll('.ratio');
    for (var j = 0; j < ratios.length; j++) ratios[j].disabled = busy;
  }

  function enterRender(snap) {
    S.renderActive = true;
    S.playing = false;
    cancelAnimationFrame(S.playRaf);
    els.playBtn.classList.remove('playing');
    els.playIcon.textContent = '▶';
    S.renderSnapshot = snap;
    S.lastFrameRev = -1;
    els.renderBtnText.textContent = '渲染中…';
    els.renderBtn.disabled = true;
    els.cancelBtn.hidden = false;
    els.cancelBtn.disabled = false;
    els.cancelBtn.textContent = '取消渲染';
    els.emptyState.hidden = true;
    els.canvasFrame.hidden = false;
    els.stageBadge.textContent = 'RENDERING';
    els.liveDot.className = 'live-dot rendering';
    els.renderScan.hidden = false;
    els.frameBadge.hidden = false;
    els.scrub.disabled = true;
    els.playBtn.disabled = true;
    els.openOutputBtn.hidden = true;
    setControlsBusy(true);
    setNote('正在由 Chromium 逐帧渲染，完成后自动编码 MP4。');
    setStatus(snap.stage || '准备渲染', 'busy');
    toast('渲染已开始', 'info', 2400);
  }

  function leaveRender(snap) {
    S.renderActive = false;
    S.renderSnapshot = null;
    els.renderBtnText.textContent = '开始渲染';
    els.cancelBtn.hidden = true;
    els.renderScan.hidden = true;
    els.frameBadge.hidden = true;
    els.stageBadge.textContent = 'PREVIEW';
    els.liveDot.className = 'live-dot on';
    els.renderFrame.hidden = true;
    els.renderFrame.removeAttribute('src');
    setControlsBusy(false);
    updatePlayState();
    updateRenderButton();
    updateTimeline();

    var finished = snap && snap.stage === '渲染完成' && !snap.error;
    if (finished) {
      if (snap.output) els.output.value = snap.output;
      els.openOutputBtn.hidden = false;
      els.stageBadge.textContent = 'DONE';
      setStatus('渲染完成 · ' + (snap.totalFrames || 0) + ' 帧', 'ok');
      els.progressText.textContent = snap.elapsed ? (Number(snap.elapsed).toFixed(1) + 's') : '';
      toast('已导出 ' + (snap.output || 'MP4 文件'), 'success', 6000);
      if (S.hasHtml) setTime(S.duration, true);
    } else if (snap && !snap.error && /取消/.test(snap.stage || '')) {
      setStatus('渲染已取消', '');
      els.progressText.textContent = '';
      setNote('已取消本次渲染。');
      toast('已取消渲染', 'info');
      if (S.hasHtml) previewSeek(S.time);
    } else {
      setStatus('渲染失败', 'bad');
      els.progressText.textContent = '';
      setNote(snap && snap.error ? snap.error : '渲染失败，请查看日志。', 'error');
      toast((snap && snap.error) || '渲染失败', 'error', 9000);
      if (S.hasHtml) previewSeek(S.time);
    }
    saveSettings();
  }

  function handleRenderSnapshot(snap) {
    S.render = snap || {};
    if (snap && snap.running) {
      if (!S.renderActive) enterRender(snap);
      S.renderSnapshot = snap;
      var t = snap.fps ? (snap.frame || 0) / snap.fps : 0;
      t = Math.min(t, snap.duration || S.duration);
      els.stageBadge.textContent = 'RENDERING';
      els.liveDot.className = 'live-dot rendering';
      els.renderScan.hidden = false;
      els.frameBadge.hidden = false;
      els.scrub.disabled = true;
      updateTimeline({ time: t, frame: snap.frame || 0, totalFrames: snap.totalFrames || TOTAL_FRAMES() });
      var pct = Math.round(clamp(snap.progress || 0, 0, 1) * 100);
      var parts = [pct + '%'];
      if (snap.speed) parts.push(Number(snap.speed).toFixed(1) + ' 帧/秒');
      if (snap.eta > 0.5) parts.push('剩余 ' + formatShort(snap.eta));
      els.progressText.textContent = parts.join(' · ');
      setStatus('正在渲染 · ' + (snap.stage || ''), 'busy');
      els.cancelBtn.textContent = '取消渲染';
      if (snap.output) els.output.value = snap.output;
      if (snap.frameRev && snap.frameRev !== S.lastFrameRev) {
        S.lastFrameRev = snap.frameRev;
        els.renderFrame.hidden = false;
        els.renderFrame.src = '/api/frame?v=' + snap.frameRev;
      }
    } else if (S.renderActive) {
      leaveRender(snap || {});
    }
  }

  function formatShort(seconds) {
    seconds = Math.max(0, Number(seconds) || 0);
    if (seconds < 60) return Math.ceil(seconds) + 's';
    var m = Math.floor(seconds / 60);
    var s = Math.round(seconds - m * 60);
    if (s === 60) { m += 1; s = 0; }
    return m + 'm' + pad2(s) + 's';
  }

  function startRender() {
    if (S.renderActive || S.busy) return;
    if (!S.hasHtml) { toast('请先选择 HTML 文件', 'error'); return; }
    if (!S.chromium || !S.chromium.available) {
      toast((S.chromium && S.chromium.error) || '没有找到 Chromium（Chrome 或 Edge）', 'error', 8000);
      return;
    }
    var width = makeEven(clamp(readInt(els.width, S.width), 64, 7680));
    var height = makeEven(clamp(readInt(els.height, S.height), 64, 7680));
    var duration = clamp(parseFloat(els.duration.value) || S.duration, 0.1, 3600);
    var fps = clamp(parseInt(els.fps.value, 10) || S.fps, 1, 120);
    S.width = width; S.height = height; S.duration = duration; S.fps = fps;

    var output = els.output.value.trim();
    if (!output) output = S.defaultOutput || '';
    if (!output) { toast('请选择 MP4 导出路径', 'error'); return; }

    // Ensure FFmpeg first; download automatically if missing.
    ensureFFmpeg().then(function (ok) {
      if (!ok) return;
      setControlsBusy(true);
      els.renderBtn.disabled = true;
      els.renderBtnText.textContent = '正在启动…';
      setNote('正在启动 Chromium…');
      return api('/api/render', {
        htmlPath: S.htmlPath,
        output: output,
        audioPath: els.audio.value.trim(),
        width: S.width,
        height: S.height,
        duration: S.duration,
        fps: S.fps
      }).then(function (snap) {
        setControlsBusy(false);
        S.renderActive = false; // force enterRender on next snapshot
        handleRenderSnapshot(snap || { running: true });
        if (!S.renderActive) {
          enterRender(snap || { running: true });
        }
        schedulePoll(120);
      }).catch(function (err) {
        setControlsBusy(false);
        els.renderBtnText.textContent = '开始渲染';
        updateRenderButton();
        setNote(err.message, 'error');
        toast(err.message, 'error', 9000);
      });
    });
  }

  function cancelRender() {
    if (!S.renderActive) return;
    els.cancelBtn.disabled = true;
    els.cancelBtn.textContent = '正在取消…';
    api('/api/cancel', {}).then(function () {
      schedulePoll(200);
    }).catch(function (err) {
      els.cancelBtn.disabled = false;
      els.cancelBtn.textContent = '取消渲染';
      toast(err.message, 'error');
    });
  }

  function ensureFFmpeg() {
    if (S.ffmpeg && S.ffmpeg.available) return Promise.resolve(true);
    if (S.ffmpeg && !S.ffmpeg.downloading) {
      downloadFFmpeg(false);
    }
    setNote('FFmpeg 未就绪，正在自动下载，完成后会自动开始渲染…');
    setStatus('正在获取 FFmpeg…', 'busy');
    var started = Date.now();
    return new Promise(function (resolve) {
      function check() {
        if (S.ffmpeg && S.ffmpeg.available) {
          setNote('FFmpeg 已就绪，开始渲染…', 'good');
          resolve(true);
          return;
        }
        if (S.ffmpeg && S.ffmpeg.error && !S.ffmpeg.downloading) {
          setNote('FFmpeg 下载失败：' + S.ffmpeg.error, 'error');
          toast('FFmpeg 下载失败：' + S.ffmpeg.error, 'error', 9000);
          resolve(false);
          return;
        }
        if (Date.now() - started > 15 * 60 * 1000) {
          resolve(false);
          return;
        }
        setTimeout(function () {
          api('/api/state').then(function (st) {
            if (st) {
              S.ffmpeg = st.ffmpeg || S.ffmpeg;
              S.chromium = st.chromium || S.chromium;
              renderChips();
              updateRenderButton();
            }
            check();
          }).catch(function () { setTimeout(check, 1200); });
        }, 700);
      }
      check();
    });
  }

  function downloadFFmpeg(notify) {
    if (S.ffmpeg && (S.ffmpeg.available || S.ffmpeg.downloading)) return;
    api('/api/ffmpeg/download', {}).then(function () {
      if (notify) toast('已开始下载 FFmpeg', 'info');
      schedulePoll(400);
    }).catch(function (err) { toast(err.message, 'error'); });
  }

  // ------------------------------------------------------------- dialogs ---

  function pick(kind) {
    return api('/api/dialog', { kind: kind }).then(function (resp) {
      if (!resp || resp.canceled || !resp.path) return null;
      return resp.path;
    }).catch(function (err) {
      toast('无法打开文件选择框：' + err.message, 'error', 7000);
      return null;
    });
  }

  // --------------------------------------------------------------- wiring ---

  function bind() {
    els.browseHtml.addEventListener('click', function () {
      pick('html').then(function (p) { if (p) applyHTMLPath(p); });
    });
    els.emptyPick.addEventListener('click', function () {
      pick('html').then(function (p) { if (p) applyHTMLPath(p); });
    });

    els.htmlPath.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') {
        e.preventDefault();
        applyHTMLPath(els.htmlPath.value);
      }
    });
    els.htmlPath.addEventListener('change', function () {
      var v = els.htmlPath.value.trim();
      if (!v || v === S.htmlPath) return;
      if (/\.html?$/i.test(v)) applyHTMLPath(v);
    });

    els.width.addEventListener('change', onDimensionsChanged);
    els.height.addEventListener('change', onDimensionsChanged);
    els.width.addEventListener('input', function () { previewDims(); });
    els.height.addEventListener('input', function () { previewDims(); });

    els.ratioRow.addEventListener('click', function (e) {
      var b = e.target.closest ? e.target.closest('.ratio') : null;
      if (!b || b.disabled) return;
      S.width = Number(b.getAttribute('data-w'));
      S.height = Number(b.getAttribute('data-h'));
      updateMeta();
      saveSettings();
    });

    els.duration.addEventListener('change', function () {
      S.duration = clamp(parseFloat(els.duration.value) || 10, 0.1, 3600);
      if (S.time > S.duration) S.time = S.duration;
      updateMeta();
      updatePlayState();
      saveSettings();
      previewSeek(S.time);
    });
    els.autoDuration.addEventListener('click', autoDuration);
    els.fps.addEventListener('change', function () {
      S.fps = clamp(parseInt(els.fps.value, 10) || 30, 1, 120);
      updateMeta();
      saveSettings();
    });

    els.output.addEventListener('input', function () {
      S.outputAuto = els.output.value.trim() === '';
      saveSettings();
    });

    els.audio.addEventListener('input', saveSettings);
    els.browseAudio.addEventListener('click', function () {
      pick('audio').then(function (p) {
        if (!p) return;
        els.audio.value = p;
        saveSettings();
      });
    });

    els.browseOutput.addEventListener('click', function () {
      pick('output').then(function (p) {
        if (!p) return;
        els.output.value = p;
        S.outputAuto = false;
        saveSettings();
      });
    });

    els.revealOutput.addEventListener('click', function () { openPath(els.output.value.trim(), true); });
    els.openOutputBtn.addEventListener('click', function () { openPath(els.output.value.trim(), false); });

    els.copyPath.addEventListener('click', function () {
      var v = els.output.value.trim();
      if (!v) return;
      var done = function () { toast('路径已复制', 'success', 1800); };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(v).then(done).catch(function () { fallbackCopy(v); done(); });
      } else {
        fallbackCopy(v);
        done();
      }
    });

    els.renderBtn.addEventListener('click', startRender);
    els.cancelBtn.addEventListener('click', cancelRender);
    els.playBtn.addEventListener('click', togglePlay);
    els.ffmpegChip.addEventListener('click', function () { downloadFFmpeg(true); });
    els.chromeChip.addEventListener('click', function () {
      if (!S.chromium || !S.chromium.available) toast((S.chromium && S.chromium.error) || '未找到 Chromium', 'error', 8000);
    });

    els.scrub.addEventListener('input', function () {
      if (S.renderActive) return;
      if (S.playing) pausePlay();
      var t = (Number(els.scrub.value) / 10000) * S.duration;
      S.time = t;
      updateTimeline();
      previewSeek(t);
    });

    els.preview.addEventListener('load', function () {
      if (!els.preview.src || /^about:blank/.test(els.preview.src)) return;
      S.iframeReady = true;
      els.stageBadge.textContent = 'PREVIEW';
      els.liveDot.className = 'live-dot on';
      updatePlayState();
      if (S.time > 0) previewSeek(S.time);
    });

    els.preview.addEventListener('error', function () {
      setStatus('预览加载失败', 'bad');
    });

    window.addEventListener('message', onPreviewMessage);

    window.addEventListener('resize', function () { fitCanvas(); });

    window.addEventListener('keydown', function (e) {
      var tag = (e.target && e.target.tagName) || '';
      if (/INPUT|SELECT|TEXTAREA/.test(tag)) return;
      if (S.renderActive || !S.hasHtml) return;
      if (e.code === 'Space') {
        e.preventDefault();
        togglePlay();
      } else if (e.code === 'ArrowLeft' || e.code === 'ArrowRight') {
        e.preventDefault();
        if (S.playing) pausePlay();
        var step = e.shiftKey ? 1 : (1 / S.fps);
        var dir = e.code === 'ArrowLeft' ? -1 : 1;
        setTime(S.time + dir * step, true);
      } else if (e.code === 'Home') {
        e.preventDefault();
        setTime(0, true);
      } else if (e.code === 'End') {
        e.preventDefault();
        setTime(S.duration, true);
      }
    });
  }

  function onDimensionsChanged() {
    S.width = makeEven(clamp(readInt(els.width, S.width), 64, 7680));
    S.height = makeEven(clamp(readInt(els.height, S.height), 64, 7680));
    if (S.time > S.duration) S.time = S.duration;
    updateMeta();
    updatePlayState();
    saveSettings();
    previewDims();
  }

  function previewDims() {
    var w = makeEven(clamp(readInt(els.width, S.width), 64, 7680));
    var h = makeEven(clamp(readInt(els.height, S.height), 64, 7680));
    els.resolutionText.textContent = w + ' × ' + h;
    updateRatioButtons();
  }

  function onPreviewMessage(e) {
    var d = e.data;
    if (!d || d.source !== 'chromaframe') return;
    if (els.preview.contentWindow && e.source !== els.preview.contentWindow) return;
    if (d.type === 'ready' || d.type === 'load' || d.type === 'dom-ready') {
      S.iframeReady = true;
      S.inflightSeeks = 0;
      if (d.duration && isFinite(d.duration)) {
        S.estimatedDuration = Number(d.duration);
        if (S.hasHtml && els.htmlHint.className.indexOf('error') === -1) {
          els.htmlHint.className = 'hint good';
          els.htmlHint.textContent = '已载入 · ' + S.htmlName + (S.estimatedDuration > 0 ? ' · 页面动画约 ' + S.estimatedDuration.toFixed(1) + 's' : '');
        }
      }
      if (d.type === 'ready') {
        els.stageBadge.textContent = 'PREVIEW';
        els.liveDot.className = 'live-dot on';
        updatePlayState();
        previewSeek(S.time);
        setStatus('预览就绪', 'ok');
      }
      return;
    }
    if (d.type === 'time-set') {
      S.inflightSeeks = Math.max(0, S.inflightSeeks - 1);
      return;
    }
    if (d.type === 'duration') {
      var waiter = S.estimateWaiters[d.id];
      if (waiter) {
        delete S.estimateWaiters[d.id];
        clearTimeout(waiter.timer);
        waiter.resolve(Number(d.duration) || 0);
      }
      return;
    }
  }

  function autoDuration() {
    if (!S.hasHtml) { toast('请先选择 HTML 文件', 'error'); return; }
    if (!S.iframeReady) { toast('页面还没有加载完成', 'error'); return; }
    var id = ++S.estimateSeq;
    var promise = new Promise(function (resolve) {
      var timer = setTimeout(function () {
        delete S.estimateWaiters[id];
        resolve(0);
      }, 2600);
      S.estimateWaiters[id] = { resolve: resolve, timer: timer };
    });
    postToPreview({ type: 'estimate-duration', id: id });
    promise.then(function (d) {
      if (!d || d < 0.05) {
        toast('未能从页面检测到动画时长，请手动填写', 'error', 6000);
        return;
      }
      S.duration = clamp(Math.ceil(d * 10) / 10, 0.1, 3600);
      if (S.time > S.duration) S.time = S.duration;
      updateMeta();
      updatePlayState();
      saveSettings();
      toast('检测到页面时长约 ' + d.toFixed(1) + ' 秒', 'success');
    });
  }

  function openPath(path, reveal) {
    if (!path) { toast('导出路径为空', 'error'); return; }
    api('/api/open', { path: path }).then(function () {
      if (reveal) toast('已打开所在位置', 'info', 2200);
    }).catch(function (err) { toast(err.message, 'error', 6000); });
  }

  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
  }

  // ----------------------------------------------------------------- init ---

  function init() {
    restoreSettings();
    S.width = makeEven(clamp(readInt(els.width, 1920), 64, 7680));
    S.height = makeEven(clamp(readInt(els.height, 1080), 64, 7680));
    S.duration = clamp(parseFloat(els.duration.value) || 10, 0.1, 3600);
    S.fps = clamp(parseInt(els.fps.value, 10) || 30, 1, 120);
    bind();
    updateMeta();
    updatePlayState();
    renderChips();
    updateRenderButton();
    fitCanvas();
    pollState();

    // Restore the last HTML file after the first state poll has provided auth.
    var saved = null;
    try { saved = JSON.parse(localStorage.getItem(SETTINGS_KEY) || 'null'); } catch (e) {}
    if (saved && typeof saved.htmlPath === 'string' && saved.htmlPath.trim()) {
      setTimeout(function () {
        if (S.htmlPath) return;
        applyHTMLPath(saved.htmlPath, { silent: true }).then(function (ok) {
          if (!ok) {
            els.htmlPath.classList.remove('invalid');
            els.htmlHint.className = 'hint';
            els.htmlHint.textContent = '上次使用的文件已不可用，请重新选择。';
          }
        });
      }, 260);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init, { once: true });
  } else {
    init();
  }
})();
