/*
 * Chromaframe time controller.
 *
 * This file is injected as the very first script of the preview page and of
 * the headless renderer. It replaces requestAnimationFrame / performance.now /
 * Date.now with a virtual clock so a frame can be requested for an exact time,
 * pauses and seeks CSS animations and media elements, and reports page load
 * state to the host UI.
 *
 * It intentionally leaves setTimeout/setInterval native: virtual timers would
 * break pages that stage their asset loading with delays, and the overwhelming
 * majority of HTML animations are driven by rAF, CSS animations or media.
 */
(function () {
  'use strict';
  if (window.__cf && window.__cf.version) return;

  var nativeRAF = window.requestAnimationFrame ? window.requestAnimationFrame.bind(window) : null;
  var nativeCAF = window.cancelAnimationFrame ? window.cancelAnimationFrame.bind(window) : null;
  var nativeSetTimeout = window.setTimeout.bind(window);
  var nativeClearTimeout = window.clearTimeout.bind(window);
  var nativeNow = (window.performance && performance.now) ? performance.now.bind(performance) : function () { return Date.now() - __cfDateBase; };
  var realStart = nativeNow();
  var dateBase = Date.now() - realStart;

  var virtualMs = 0;
  var rafCallbacks = new Map();
  var nextRafId = 1;
  var animationStarts = new WeakMap();
  var chain = Promise.resolve();
  var firstApply = true;

  // Parse ?__cf_t=1.25 or #__cf_t=1.25 from the URL.
  (function () {
    var m = /[?&#]__cf_t=([0-9]+(?:\.[0-9]+)?)/.exec(location.href);
    if (m) {
      var v = parseFloat(m[1]);
      if (isFinite(v) && v >= 0) virtualMs = v * 1000;
    }
  })();

  function report(type, extra) {
    if (window.parent && window.parent !== window) {
      try {
        var msg = { source: 'chromaframe', type: type };
        if (extra) for (var k in extra) if (Object.prototype.hasOwnProperty.call(extra, k)) msg[k] = extra[k];
        window.parent.postMessage(msg, '*');
      } catch (e) { /* cross-origin host is fine */ }
    }
  }

  function runFrameCallbacks() {
    if (!rafCallbacks.size) return;
    var pending = [];
    rafCallbacks.forEach(function (cb) { pending.push(cb); });
    rafCallbacks.clear();
    for (var i = 0; i < pending.length; i++) {
      try { pending[i](virtualMs); } catch (e) { console.error('[chromaframe] rAF callback failed', e); }
    }
  }

  function applyAnimations() {
    if (!document.getAnimations) return;
    var list;
    try { list = document.getAnimations(); } catch (e) { return; }
    for (var i = 0; i < list.length; i++) {
      var anim = list[i];
      try {
        var start = animationStarts.get(anim);
        if (start === undefined) {
          start = firstApply ? 0 : virtualMs;
          animationStarts.set(anim, start);
        }
        var local = virtualMs - start;
        if (local < 0) local = 0;
        anim.pause();
        anim.currentTime = local;
      } catch (e) { /* animation may be detached */ }
    }
  }

  function seekMedia() {
    var media = document.querySelectorAll('video, audio');
    if (!media.length) return Promise.resolve();
    var waits = [];
    for (var i = 0; i < media.length; i++) {
      (function (el) {
        try { el.pause(); } catch (e) {}
        waits.push(new Promise(function (resolve) {
          var target = virtualMs / 1000;
          if (isFinite(el.duration) && el.duration > 0) {
            target = Math.min(target, Math.max(0, el.duration - 0.001));
          }
          if (el.readyState < 1) {
            var finished = false;
            var timer = nativeSetTimeout(function () { finish(); }, 1200);
            function finish() {
              if (finished) return;
              finished = true;
              nativeClearTimeout(timer);
              resolve();
            }
            el.addEventListener('loadedmetadata', function () {
              seekTo(el, target);
              nativeSetTimeout(finish, 0);
            }, { once: true });
            el.addEventListener('error', finish, { once: true });
            return;
          }
          seekTo(el, target);
          var done = false;
          function finish() { if (!done) { done = true; resolve(); } }
          if (Math.abs(el.currentTime - target) <= 0.002) { finish(); return; }
          el.addEventListener('seeked', finish, { once: true });
          nativeSetTimeout(finish, 600);
        }));
      })(media[i]);
    }
    return Promise.all(waits);
  }

  function seekTo(el, target) {
    try { el.currentTime = target; } catch (e) {}
  }

  function waitImages() {
    var imgs = document.images;
    if (!imgs.length) return Promise.resolve();
    var waits = [];
    for (var i = 0; i < imgs.length; i++) {
      var img = imgs[i];
      if (!img.complete) {
        waits.push(new Promise(function (resolve) {
          img.addEventListener('load', function () { resolve(); }, { once: true });
          img.addEventListener('error', function () { resolve(); }, { once: true });
          nativeSetTimeout(resolve, 1500);
        }));
      }
    }
    return waits.length ? Promise.all(waits) : Promise.resolve();
  }

  function nextPaint() {
    return new Promise(function (resolve) {
      if (!nativeRAF) { nativeSetTimeout(resolve, 8); return; }
      nativeRAF(function () { nativeRAF(function () { resolve(); }); });
    });
  }

  function applyTime(seconds) {
    virtualMs = Math.max(0, Number(seconds) * 1000 || 0);
    runFrameCallbacks();
    applyAnimations();
    return seekMedia()
      .then(waitImages)
      .then(nextPaint)
      .then(function () {
        firstApply = false;
        return virtualMs / 1000;
      });
  }

  function setTime(seconds) {
    chain = chain.then(function () { return applyTime(seconds); });
    return chain;
  }

  function estimateDuration() {
    var max = 0;
    try {
      var list = document.getAnimations ? document.getAnimations() : [];
      for (var i = 0; i < list.length; i++) {
        var timing = list[i].effect && list[i].effect.getComputedTiming ? list[i].effect.getComputedTiming() : null;
        if (!timing) continue;
        var end = timing.endTime;
        if (typeof end === 'number' && isFinite(end) && end > max) max = end;
      }
    } catch (e) {}
    var media = document.querySelectorAll('video, audio');
    for (var j = 0; j < media.length; j++) {
      var d = media[j].duration;
      if (isFinite(d) && d * 1000 > max) max = d * 1000;
    }
    var explicit = parseFloat(document.documentElement.getAttribute('data-cf-duration'));
    if (isFinite(explicit) && explicit > max) max = explicit * 1000;
    if (!max) max = 10000;
    return Math.max(0.5, Math.min(3600, max / 1000));
  }

  // Replace the browser clock seen by the page.
  try { window.requestAnimationFrame = function (cb) { var id = nextRafId++; rafCallbacks.set(id, cb); return id; }; } catch (e) {}
  try { window.cancelAnimationFrame = function (id) { rafCallbacks.delete(id); }; } catch (e) {}
  try {
    if (window.performance) {
      Object.defineProperty(window.performance, 'now', {
        configurable: true,
        writable: true,
        value: function () { return virtualMs; }
      });
    }
  } catch (e) {
    try { if (window.performance) window.performance.now = function () { return virtualMs; }; } catch (e2) {}
  }
  try {
    Date.now = function () { return Math.floor(dateBase + virtualMs); };
  } catch (e) {}

  window.__cf = {
    version: 1,
    setTime: setTime,
    getTime: function () { return virtualMs / 1000; },
    estimateDuration: estimateDuration,
    isReady: function () { return document.readyState === 'complete'; },
    pendingRequests: 0
  };

  window.addEventListener('message', function (e) {
    if (e.source !== window.parent) return;
    var data = e.data;
    if (!data || data.source !== 'chromaframe-host') return;
    if (data.type === 'set-time') {
      setTime(data.time).then(function (t) {
        report('time-set', { id: data.id, time: t });
      });
    } else if (data.type === 'get-time') {
      report('time', { id: data.id, time: virtualMs / 1000, duration: estimateDuration() });
    } else if (data.type === 'estimate-duration') {
      report('duration', { id: data.id, duration: estimateDuration() });
    } else if (data.type === 'ping') {
      report('pong', { id: data.id });
    }
  });

  function announceReady() {
    report('ready', {
      duration: estimateDuration(),
      headless: /[?&]__cf_headless=1/.test(location.search),
      title: document.title || ''
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      report('dom-ready', { duration: estimateDuration() });
    }, { once: true });
  } else {
    nativeSetTimeout(function () { report('dom-ready', { duration: estimateDuration() }); }, 0);
  }
  window.addEventListener('load', function () {
    report('load', { duration: estimateDuration() });
    announceReady();
  }, { once: true });
  if (document.readyState === 'complete') {
    nativeSetTimeout(announceReady, 0);
  }
})();
