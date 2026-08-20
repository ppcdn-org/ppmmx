/**
 * utils-v1.0.3.js
 * 通用工具函数、网络助手、DOM 操作、能力检测
 */

/* ------------------------
   CONFIG / 全局参数
   ------------------------ */
const CONFIG_API = {
  API_GET_WEBRTC_URL: '/api/play/txUrl',
  API_START_PLAY_URL: '/play/start',
  API_STOP_PLAY_URL: '/play/end',
  API_PLAY_LAG_URL: '/lag'
};

const SNAPSHOT_STORE_INTERVAL_MS = 120000;
const FALLBACK_SNAPSHOT_STORAGE_KEY = 'abrplayer:fallbackSnapshot';
const FALLBACK_SNAPSHOT_MAX_SIDE = 960;

const STATAPI_CONFIG_URL = '/api/settings/app-env';
const STATAPI_CONFIG = {
    env: 'test',
    baseUrl: '',
    ready: false
};
let statapiConfigPromise = null;
let snapshotStoreTimer = null;
let lastSnapshotStoreAt = 0;
let statapiDisabledLogged = false;

function joinBaseURL(baseUrl, path) {
    const base = String(baseUrl || '').replace(/\/+$/, '');
    const suffix = String(path || '').startsWith('/') ? String(path || '') : `/${path || ''}`;
    return base ? `${base}${suffix}` : suffix;
}

function statApiURL(path) {
    return STATAPI_CONFIG.baseUrl ? joinBaseURL(STATAPI_CONFIG.baseUrl, path) : '';
}

function shouldSkipStatApiPost(action) {
    if (STATAPI_CONFIG.baseUrl) return false;
    if (!statapiDisabledLogged) {
        console.warn(`[StatAPI] disabled: missing baseUrl, skip ${action}.`);
        statapiDisabledLogged = true;
    }
    return true;
}

async function initStatApiConfig(force) {
    if (STATAPI_CONFIG.ready && !force) return STATAPI_CONFIG;
    if (statapiConfigPromise && !force) return statapiConfigPromise;

    statapiConfigPromise = (async function() {
        try {
            const response = await fetch(STATAPI_CONFIG_URL, { method: 'GET', cache: 'no-store' });
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const result = await response.json();
            const data = result && result.data ? result.data : {};
            const domain = String(data.baseUrl || data.domain || '').trim().replace(/\/+$/, '');
            if (!domain) throw new Error('empty statapi domain');
            STATAPI_CONFIG.env = String(data.env || STATAPI_CONFIG.env || 'test').toLowerCase();
            STATAPI_CONFIG.baseUrl = domain;
            STATAPI_CONFIG.ready = true;
            console.log(`[StatAPI] env=${STATAPI_CONFIG.env}, baseUrl=${STATAPI_CONFIG.baseUrl}`);
        } catch (err) {
            STATAPI_CONFIG.ready = true;
            console.warn(`[StatAPI] using same-origin API fallback: ${err && err.message ? err.message : err}`);
        } finally {
            statapiConfigPromise = null;
        }
        return STATAPI_CONFIG;
    })();

    return statapiConfigPromise;
}

/* ------------------------
   基础工具
   ------------------------ */
function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

function debounce(fn, wait) {
    let t;
    return function (...args) {
        clearTimeout(t);
        t = setTimeout(() => fn.apply(this, args), wait);
    }
}

/* ------------------------
   Cookie / URL Helpers
   ------------------------ */
function getCookie(name) {
    var m = document.cookie.match('(^|;)\\s*' + name + '\\s*=\\s*([^;]+)');
    return m ? decodeURIComponent(m.pop()) : null;
}

function setCookie(name, value, days) {
    var expires = '';
    if (days) {
        var d = new Date(); d.setTime(d.getTime() + (days * 24 * 60 * 60 * 1000));
        expires = '; expires=' + d.toUTCString();
    }
    document.cookie = name + '=' + encodeURIComponent(value) + expires + '; path=/';
}

function getStreamNameFromUrl(url) {
    if (!url) return null;
    const match = url.match(/\/(live\/[^?]+)/);
    return match ? match[1] : null;
}

function getBareStreamNameFromUrl(url) {
    const streamName = getStreamNameFromUrl(url);
    return streamName ? streamName.replace(/^live\//, '') : null;
}

function cacheWebrtcUrlForStream(stream, url) {
    if (!stream || !url) return;
    try {
        sessionStorage.setItem(`abrplayer:webrtc:${stream}`, url);
    } catch (e) {}
}

function getCachedWebrtcUrlForStream(stream) {
    if (!stream) return null;
    try {
        const cached = sessionStorage.getItem(`abrplayer:webrtc:${stream}`);
        if (cached && getBareStreamNameFromUrl(cached) === stream) return cached;
    } catch (e) {}

    const cookieUrl = getCookie('phil_stream_url');
    if (cookieUrl && getBareStreamNameFromUrl(cookieUrl) === stream) return cookieUrl;
    return null;
}

function normalizeSnapshotStreamName(streamName) {
    const value = String(streamName || '').trim();
    if (!value) return '';
    return value
        .replace(/_standard_hevc$/i, '')
        .replace(/_standard$/i, '')
        .replace(/_economic$/i, '')
        .replace(/_audio$/i, '');
}

function buildCanvasProfile(width, height) {
    width = Number(width) || 720;
    height = Number(height) || 1280;
    return {
        width: width,
        height: height,
        aspectRatio: `${width} / ${height}`,
        aspectW: width,
        aspectH: height,
        maxWidth: `${width}px`,
        quality: `${width}x${height}`
    };
}

let currentCanvasProfile = buildCanvasProfile(720, 1280);

function getCanvasProfile(streamOrUrl) {
    const streamName = streamOrUrl && streamOrUrl.includes('://')
        ? getStreamNameFromUrl(streamOrUrl)
        : streamOrUrl;

    if (streamName && (streamName.includes('-fwh') || streamName.includes('_fwh'))) {
        return buildCanvasProfile(1280, 720);
    }

    return buildCanvasProfile(720, 1280);
}

function resolveCanvasProfile(arg1, arg2) {
    if (typeof arg1 === 'number' && typeof arg2 === 'number' && arg1 > 0 && arg2 > 0) {
        return buildCanvasProfile(arg1, arg2);
    }

    if (arg1 && typeof arg1 === 'object') {
        const width = arg1.videoWidth || arg1.width || 0;
        const height = arg1.videoHeight || arg1.height || 0;
        if (width > 0 && height > 0) {
            return buildCanvasProfile(width, height);
        }
    }

    return getCanvasProfile(arg1);
}

function updateCanvasProfile(arg1, arg2) {
    currentCanvasProfile = resolveCanvasProfile(arg1, arg2);
    applyCanvasLayout(currentCanvasProfile);
    return currentCanvasProfile;
}

function getDomainFromUrlRegex(urlStr) {
    if (!urlStr) return null;
    // 匹配 :// 之后，直到遇到下一个 / 或者字符串结束
    const match = urlStr.match(/:\/\/(.[^/]+)/);
    if (match && match[1]) {
        // 移除可能存在的端口号 (例如 domain.com:8080 -> domain.com)
        return match[1].split(':')[0];
    }
    return null;
}

/* ------------------------
   Capability Detection (HEVC)
   ------------------------ */
let _hevcSupportCached = null;

async function detectHevcCapability() {
    if (_hevcSupportCached !== null) return _hevcSupportCached;
    let supported = false;
    if (navigator.mediaCapabilities && navigator.mediaCapabilities.decodingInfo) {
        try {
            const info = await navigator.mediaCapabilities.decodingInfo({
                type: "file",
                video: {
                    contentType: 'video/mp4; codecs="hvc1.1.6.L93.B0"',
                    width: 1280, height: 720, bitrate: 2000000, framerate: 30,
                },
            });
            supported = info.supported;
            console.log(`[Capability] MediaCapabilities check: ${supported}`);
        } catch (e) {
            supported = internalCanPlayHevc();
        }
    } else {
        supported = internalCanPlayHevc();
    }
    _hevcSupportCached = supported;
    console.log(`[Capability] HEVC Support: ${_hevcSupportCached}`);
    return _hevcSupportCached;
}

function internalCanPlayHevc() {
    const v = document.createElement("video");
    return v.canPlayType('video/mp4; codecs="hvc1"').replace(/^no$/, '') !== '';
}

function supportsHEVC() {
    if (_hevcSupportCached === null) return false;
    return _hevcSupportCached;
}

/* ------------------------
   Network Helpers
   ------------------------ */
async function getWebrtcUrl(stream) {
    let webrtcUrl = getCachedWebrtcUrlForStream(stream);
    if (!stream) return webrtcUrl;
    const url = `${CONFIG_API.API_GET_WEBRTC_URL}?stream=${encodeURIComponent(stream)}`;
    try {
        const response = await fetch(url, {
            method: 'GET',
            headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer 123@VideoStat' },
            cache: 'no-store'
        });
        if (!response.ok) return webrtcUrl;
        const result = await response.json();
        if (result && result.code === 0 && result.data) {
            if (result.data.streams && result.data.streams.length > 0) {
                const fallbackUrl = result.data.streams[0].webrtc;
                console.log(`[PlayURL] ${stream} -> ${fallbackUrl}`);
                cacheWebrtcUrlForStream(stream, fallbackUrl);
                return fallbackUrl;
            }
            if (result.data.webrtc) {
                console.log(`[PlayURL] ${stream} -> ${result.data.webrtc}`);
                cacheWebrtcUrlForStream(stream, result.data.webrtc);
                return result.data.webrtc;
            }
        }
        return webrtcUrl;
    } catch (error) {
        return webrtcUrl;
    }
}

/**
 * 通用测速请求执行器
 * @param {string} url - 测速地址
 * @returns {number} kbps (失败返回 0)
 */
async function probeBandwidth(url) {
    let received = 0;
    
    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 5000);
    let startTime = performance.now();
    try {
        const response = await fetch(url, { 
            method: 'GET', 
            signal: controller.signal,
            cache: 'no-store'
        });

        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        if (!response.body) throw new Error('No body');

        const reader = response.body.getReader();
        while(true) {
            const {done, value} = await reader.read();
            if (done) break;
            received += value.length;
            if (received >= 122880) { // 120KB
                controller.abort();
                break;
            }
        }
    } catch(e) {
        if (received < 1024) { 
            if (e.name !== 'AbortError') {
                console.warn(`[BW] probe failed: ${e.message}.`);
            }
            clearTimeout(timeoutId);
            return null; 
        }
    }
    
    clearTimeout(timeoutId);
    const elapsed = performance.now() - startTime;
    if (elapsed <= 0 || received <= 0) return null;

    return (received*8/elapsed).toFixed(1);
}

/* ------------------------
   Visual / DOM Helpers
   ------------------------ */
function createVideoElementIfMissing() {
    const parentNode = document.getElementById('local-video');
    let videoElement = document.getElementById('player-container-id');
    if (videoElement) return videoElement;
    
    // 清理旧的
    if (parentNode) {
        const existing = parentNode.querySelectorAll('video');
        existing.forEach(v => v.remove());
        
        videoElement = document.createElement('video');
        videoElement.setAttribute('id', 'player-container-id');
        videoElement.setAttribute('playsinline', '');
        videoElement.setAttribute('webkit-playsinline', '');
        parentNode.appendChild(videoElement);
    }
    return videoElement;
}

function applyCanvasLayout(streamOrUrl) {
    const container = document.getElementById('local-video');
    const profile = resolveCanvasProfile(streamOrUrl);

    document.documentElement.style.maxWidth = '100%';
    document.body.style.maxWidth = '100%';

    if (!container) return;

    container.style.setProperty('--video-aspect-w', profile.aspectW || profile.width);
    container.style.setProperty('--video-aspect-h', profile.aspectH || profile.height);
    container.style.setProperty('--video-max-width', profile.maxWidth);
    container.style.aspectRatio = '';
    container.style.maxWidth = '';
}

function getSnapshotCanvas(videoElement) {
    let canvas = document.getElementById('snapshot-canvas');
    if (!canvas) {
        canvas = document.createElement('canvas');
        canvas.id = 'snapshot-canvas';
        canvas.style.position = 'absolute';
        canvas.style.top = '0';
        canvas.style.left = '0';
        canvas.style.width = '100%';
        canvas.style.height = '100%';
        canvas.style.zIndex = '2'; 
        canvas.style.pointerEvents = 'none'; 
        canvas.style.objectFit = 'contain';
        
        if (videoElement.parentNode) {
            videoElement.parentNode.appendChild(canvas);
        }
    }
    return canvas;
}

function getActiveVideoElement() {
    return document.querySelector('#player-container-id video') || document.querySelector('video');
}

async function captureAndStoreSnapshot(streamName, force) {
    const baseStream = normalizeSnapshotStreamName(streamName);
    if (!baseStream) return false;
    const now = Date.now();
    if (!force && now - lastSnapshotStoreAt < SNAPSHOT_STORE_INTERVAL_MS) return false;

    const vid = getActiveVideoElement();
    if (!vid || vid.videoWidth <= 0 || vid.videoHeight <= 0) return false;

    try {
        const canvas = document.createElement('canvas');
        const scale = Math.min(1, FALLBACK_SNAPSHOT_MAX_SIDE / Math.max(vid.videoWidth, vid.videoHeight));
        canvas.width = Math.max(1, Math.round(vid.videoWidth * scale));
        canvas.height = Math.max(1, Math.round(vid.videoHeight * scale));
        const ctx = canvas.getContext('2d');
        ctx.drawImage(vid, 0, 0, canvas.width, canvas.height);
        const dataUrl = canvas.toDataURL('image/jpeg', 0.72);
        if (!dataUrl || !dataUrl.startsWith('data:image/jpeg;base64,')) return false;
        localStorage.setItem(FALLBACK_SNAPSHOT_STORAGE_KEY, JSON.stringify({
            streamName: baseStream,
            source: streamName,
            width: canvas.width,
            height: canvas.height,
            image: dataUrl,
            updatedAt: new Date().toISOString()
        }));
        lastSnapshotStoreAt = now;
        console.log(`[Snapshot] stored local fallback stream=${baseStream}, source=${streamName}, resolution=${canvas.width}x${canvas.height}`);
        return true;
    } catch (err) {
        console.warn(`[Snapshot] local store failed: ${err && err.message ? err.message : err}`);
        return false;
    }
}

function startSnapshotStoreLoop(streamName) {
    stopSnapshotStoreLoop();
    captureAndStoreSnapshot(streamName, true);
    snapshotStoreTimer = setInterval(() => {
        captureAndStoreSnapshot(streamName, false);
    }, SNAPSHOT_STORE_INTERVAL_MS);
}

function stopSnapshotStoreLoop() {
    if (snapshotStoreTimer) {
        clearInterval(snapshotStoreTimer);
        snapshotStoreTimer = null;
    }
}

async function renderAudioFallbackSnapshot(streamName) {
    const videoElement = createVideoElementIfMissing() || getActiveVideoElement();
    if (!videoElement) return false;
    try {
        const raw = localStorage.getItem(FALLBACK_SNAPSHOT_STORAGE_KEY);
        if (!raw) throw new Error('local snapshot not found');
        const snapshot = JSON.parse(raw);
        if (!snapshot || !snapshot.image) throw new Error('local snapshot invalid');
        const requestedStream = normalizeSnapshotStreamName(streamName);
        if (snapshot.streamName && requestedStream && snapshot.streamName !== requestedStream) {
            console.log(`[Snapshot] using last local fallback from ${snapshot.streamName} for audio stream=${requestedStream}`);
        }
        const image = await new Promise((resolve, reject) => {
            const img = new Image();
            img.onload = () => resolve(img);
            img.onerror = () => reject(new Error('snapshot image decode failed'));
            img.src = snapshot.image;
        });
        const canvas = getSnapshotCanvas(videoElement);
        const width = image.naturalWidth || snapshot.width || currentCanvasProfile.width || 720;
        const height = image.naturalHeight || snapshot.height || currentCanvasProfile.height || 1280;
        canvas.width = width;
        canvas.height = height;
        const ctx = canvas.getContext('2d');
        ctx.drawImage(image, 0, 0, width, height);
        canvas.style.opacity = '1';
        canvas.style.display = 'block';
        console.log(`[Snapshot] audio fallback local snapshot rendered stream=${snapshot.streamName || 'unknown'}, resolution=${width}x${height}`);
        return true;
    } catch (err) {
        console.warn(`[Snapshot] audio fallback local snapshot unavailable: ${err && err.message ? err.message : err}`);
        return false;
    }
}

function freezeLastFrame() {
    const vid = getActiveVideoElement();
    if (!vid || vid.videoWidth === 0) return;
    const canvas = getSnapshotCanvas(vid);
    const profile = updateCanvasProfile(vid);
    canvas.width = profile.width;
    canvas.height = profile.height;
    const ctx = canvas.getContext('2d');
    ctx.drawImage(vid, 0, 0, canvas.width, canvas.height);
    canvas.style.opacity = '1';
    canvas.style.display = 'block';
    console.log('[UI] Last frame frozen on canvas.');
}

function unfreezeLastFrame() {
    const canvas = document.getElementById('snapshot-canvas');
    if (canvas && canvas.style.display !== 'none') {
        canvas.style.opacity = '0';
        setTimeout(() => {
            canvas.style.display = 'none';
            if(canvas.parentNode) canvas.parentNode.removeChild(canvas);
            console.log('[UI] Canvas removed, new stream visible.');
        }, 350);
    }
}

/* ------------------------
   API helpers
   ------------------------ */
let tickRound = 70000;
async function ApiPostStartPlay(webrtcSource, isSucceed, reason, loadTime, intervalFromLastPlay, canHevc) {
    try {
        await initStatApiConfig();
        if (shouldSkipStatApiPost('play/start')) return;
        tickRound++;
        const profile = currentCanvasProfile;
        const response = await fetch(statApiURL(CONFIG_API.API_START_PLAY_URL), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer 123@VideoStat' },
            body: JSON.stringify({
                "gameTypeId": "pick2win10001",
                "gameUserId": "TEST1000",
                "gameRound": `Round2025120${tickRound}`,
                "streamName": getStreamNameFromUrl(webrtcSource),
                "cdnName": getDomainFromUrlRegex(webrtcSource),
                "protocol": "webrtc",
                "quality": profile.quality,
                "canHevc": canHevc,
                "userAgent": navigator.userAgent,
                "isSucceed": isSucceed,
                "reason": reason,
                "intervalFromLastPlay": intervalFromLastPlay,
                "waitMiliTime": loadTime,
                "retryCount": 0
            })
        });
        if (!response.ok) throw new Error(`HTTP error: ${response.status}`);
        await response.json();
    } catch (err) { console.error('ApiPostStartPlay error', err && err.message); }
}

async function ApiPostEndPlay(webrtcSource, playDuration) {
    try {
        await initStatApiConfig();
        if (shouldSkipStatApiPost('play/end')) return;
        console.log(`[StatAPI] play/end duration=${playDuration}ms`);
        const response = await fetch(statApiURL(CONFIG_API.API_STOP_PLAY_URL), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer 123@VideoStat' },
            body: JSON.stringify({
                "gameTypeId": "pick2win10001",
                "gameUserId": "TEST1000",
                "gameRound": `Round2025120${tickRound}`,
                "streamName": getStreamNameFromUrl(webrtcSource),
                "cdnName": getDomainFromUrlRegex(webrtcSource),
                "playDuration": playDuration
            })
        });
        if (!response.ok) throw new Error(`HTTP error: ${response.status}`);
        await response.json();
    } catch (err) { console.error('ApiPostEndPlay error', err && err.message); }
}

async function ApiPostPlayLag(webrtcSource, lagDuration) {
    try {
        await initStatApiConfig();
        if (shouldSkipStatApiPost('lag')) return;
        const response = await fetch(statApiURL(CONFIG_API.API_PLAY_LAG_URL), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer 123@VideoStat' },
            body: JSON.stringify({
                "gameTypeId": "pick2win10001",
                "gameUserId": "TEST1000",
                "gameRound": `Round2025120${tickRound}`,
                "streamName": getStreamNameFromUrl(webrtcSource),
                "cdnName": getDomainFromUrlRegex(webrtcSource),
                "lagDuration": lagDuration
            })
        });
        if (!response.ok) throw new Error(`HTTP error: ${response.status}`);
        await response.json();
    } catch (err) { console.error('ApiPostPlayLag error', err && err.message); }
}
