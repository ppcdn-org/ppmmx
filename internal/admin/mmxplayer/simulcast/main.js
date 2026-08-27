// main.js - Adapted for tx UI
// Supports both tx HTML (#player-container-id, #quality-select)
// and legacy mmx HTML (#video, #layerSelect)

const urlInput = document.getElementById('webrtc') || document.getElementById('urlInput');
const video = document.getElementById('player-container-id') || document.getElementById('video');
const statsContainer = document.querySelector('#local-video .stat ul') || document.getElementById('stats');
const layerSelect = document.getElementById('quality-select') || document.getElementById('layerSelect');
const wsStatusDot = document.getElementById('wsStatus');
(() => {
    if (!wsStatusDot) {
        const dot = document.createElement('span');
        dot.id = 'wsStatus';
        dot.style.cssText = 'position:fixed;top:10px;right:10px;width:12px;height:12px;border-radius:50%;z-index:9999';
        document.body.appendChild(dot);
    }
    if (!urlInput) {
        const inp = document.createElement('input');
        inp.id = 'webrtc';
        inp.type = 'text';
        inp.value = 'http://localhost:8889/live/table1-fwv/whep';
        inp.style.display = 'none';
        document.body.appendChild(inp);
    }
})();

let reader = null;
let readerGeneration = 0;
let seiReaderAttached = false;
let controlClient = null;
let statsInterval = null;
let lastStats = { videoBytes: 0, audioBytes: 0, timestamp: 0 };
let previousTrackType = null; // Track if we were in audio-only mode
let lastVideoTrackId = null;
let statConfig = null;
let statStartTime = 0;
let statRound = 70000;
let statLagStartedAt = 0;
let lastLagReportAt = 0;

// p2p delay: local render time minus the timestamp the OBS publisher
// embedded when it sent the frame (see docs/obs-abs-timestamp-protocol.md
// in the OBS repo). Two independent sources feed the same displayed value:
//
// 1. SEI (sei-timestamp.js): read directly out of the encoded H.264
//    bitstream via WebCodecs Insertable Streams. Survives mmx-to-mmx
//    cascading (no server-side relay needed) and is inherently correct
//    for whatever layer is actually being decoded. Chromium-only.
// 2. DataChannel relay (OBS_TIMESTAMP over the ABR control WebSocket, see
//    obs_timestamp_broadcast.go in mmx): only valid for a direct
//    OBS->mmx->player hop (no cascading), and needs manual rid matching
//    since it carries every simulcast layer's messages. Kept as a
//    fallback for non-Chromium browsers where SEI reading isn't available.
//
// SEI takes priority whenever both are reporting fresh values.
let lastP2PDelayMs = null;
let lastP2PDelayAt = 0;
let lastP2PDelaySource = null; // 'sei' | 'datachannel'
const P2P_DELAY_STALE_MS = 5000;
// Measured delay is a floor (clock skew / rounding always trend it low,
// never high) - pad it so displayed numbers don't read as falsely great.
const P2P_DELAY_ERROR_MARGIN_MS = 50;
// The SEI/DataChannel measurement is `Date.now() - timestamp`, i.e. it's
// only valid if the player's local clock is reasonably close to the OBS
// publisher's NTP-calibrated clock. A player whose OS clock isn't NTP
// synced can be off by hundreds of ms to seconds, producing measured
// values that read as near-zero or negative (real p2p delay is never
// under ~100ms in practice - encode+network+jitter buffer alone exceed
// that). Below this floor, the measurement is clock skew, not delay, so
// fall back to the RTT/jitter-buffer-based estimate instead of showing
// a nonsensical number.
const P2P_DELAY_CLOCK_SUSPECT_MS = 100;

function reportP2PDelay(timestampMs, source) {
    // SEI is strictly more accurate (in-band, cascade-safe, no rid
    // ambiguity) - once it's reporting, ignore stale DataChannel values.
    if (source === 'datachannel' && lastP2PDelaySource === 'sei' &&
        (Date.now() - lastP2PDelayAt) < P2P_DELAY_STALE_MS) {
        return;
    }
    lastP2PDelayMs = Date.now() - Number(timestampMs) + P2P_DELAY_ERROR_MARGIN_MS;
    lastP2PDelayAt = Date.now();
    lastP2PDelaySource = source;
}

async function loadStatConfig() {
    if (statConfig) return statConfig;
    try {
        const response = await fetch('/api/settings/app-env', { cache: 'no-store' });
        const data = await response.json();
        statConfig = data && data.data ? data.data.baseUrl : '';
        console.log(`[StatAPI] configured: ${statConfig || 'disabled'}`);
    } catch (e) {
        statConfig = '';
        console.warn('[StatAPI] config failed:', e.message);
    }
    return statConfig;
}

function statURL(path) {
    return `${String(statConfig || '').replace(/\/+$/, '')}${path}`;
}

function statPayload(extra) {
    const url = urlInput && urlInput.value ? urlInput.value : '';
    const stream = (url.match(/\/live\/([^?]+)/) || [])[1] || '';
    const host = (() => { try { return new URL(url).host; } catch (e) { return ''; } })();
    return Object.assign({
        gameTypeId: 'pick2win10001', gameUserId: 'TEST1000',
        gameRound: `Round2025120${statRound}`, streamName: `live/${stream}`,
        cdnName: host, protocol: 'webrtc', userAgent: navigator.userAgent
    }, extra || {});
}

async function postStat(path, payload) {
    if (!statConfig) await loadStatConfig();
    if (!statConfig) {
        console.warn(`[StatAPI] skipped ${path}: no base URL`);
        return;
    }
    const target = statURL(path);
    console.log(`[StatAPI] POST ${target}`);
    try {
        const response = await fetch(target, { method: 'POST', headers: {'Content-Type':'application/json','Authorization':'Bearer 123@VideoStat'}, body: JSON.stringify(payload) });
        console.log(`[StatAPI] ${path} -> HTTP ${response.status}`);
    } catch (e) { console.warn('[StatAPI]', path, e.message); }
}

function statPlayStarted() {
    if (statStartTime) return;
    statStartTime = Date.now();
    statRound++;
    loadStatConfig().then(() => postStat('/play/start', statPayload({ isSucceed: true, reason: 'ok', waitMiliTime: 0, quality: 'simulcast' })));
}

function statPlayEnded() {
    if (!statStartTime) return;
    const playDuration = Date.now() - statStartTime;
    console.log(`[StatAPI] play/end duration=${playDuration}ms`);
    postStat('/play/end', statPayload({ playDuration }));
    statStartTime = 0;
}

function reportPlaybackLag(duration, source) {
    if (!statStartTime || duration < 1000) return;
    const now = Date.now();
    if (now - lastLagReportAt < 3000) return;
    lastLagReportAt = now;
    console.log(`[StatAPI] lag duration=${duration}ms source=${source}`);
    postStat('/lag', statPayload({ lagDuration: duration }));
}

// 实例化 ABR 引擎
const abrEngine = new ABREngine({
    onSwitchLayer: (trackId, reason) => {
        if (controlClient) {
            console.log(`[Main] ABR Triggered Switch: ${trackId} (${reason})`);
            switchMediaTrack(trackId, reason);
        }
    },
    onLag: (duration) => reportPlaybackLag(duration, 'abr')
});

function switchMediaTrack(trackId, reason) {
    if (!controlClient) return;
    if (trackId === abrEngine.audioTrackId) {
        if (abrEngine.videoTrackIds.includes(abrEngine.currentTrackId)) lastVideoTrackId = abrEngine.currentTrackId;
        controlClient.setMediaState({ video: 'paused', audio: 'resumed' });
        abrEngine.notifyLayerSwitched(trackId);
        previousTrackType = 'audio';
        return;
    }
    if (trackId === abrEngine.currentTrackId && !videoPaused) {
        console.log(`[Main] Ignore redundant layer switch: ${trackId} (${reason})`);
        return;
    }
    lastVideoTrackId = trackId;
    if (videoPaused) {
        // Audio-only pauses video but keeps the selector on the last video
        // layer. Resume media first; select only when the target differs.
        controlClient.setMediaState({ video: 'resumed' });
        videoPaused = false;
        if (trackId === lastVideoTrackId) {
            abrEngine.notifyLayerSwitched(trackId);
            previousTrackType = 'video';
            return;
        }
    }
    controlClient.selectLayer(trackId, reason);
}

// [修复] 视频分辨率监控：增加判重逻辑
let lastWidth = 0;
let lastHeight = 0;

video.addEventListener('resize', () => {
    const w = video.videoWidth;
    const h = video.videoHeight;
    
    // 如果尺寸没变，忽略（过滤掉 metadata 加载或浏览器内部重绘触发的 resize）
    if (w === lastWidth && h === lastHeight) return;
    
    lastWidth = w;
    lastHeight = h;

    const msg = `[Video] Resolution Changed: ${w}x${h}`;
    console.log(`%c${msg}`, 'background: #222; color: #bada55; font-size: 16px; padding: 4px; border-radius: 4px;');
    // showToast(msg);
});

video.addEventListener('loadedmetadata', () => {
    console.log(`[Video] Metadata loaded. Initial size: ${video.videoWidth}x${video.videoHeight}`);
});

// ✅ NEW: Monitor video state for debugging
video.addEventListener('waiting', () => {
    console.log('[Video] State: WAITING (buffering)');
    if (!statLagStartedAt) statLagStartedAt = Date.now();
});

video.addEventListener('playing', () => {
    console.log('[Video] State: PLAYING');
    statPlayStarted();
    if (statLagStartedAt) {
        reportPlaybackLag(Date.now() - statLagStartedAt, 'media-waiting');
        statLagStartedAt = 0;
    }
});

video.addEventListener('pause', () => {
    console.log('[Video] State: PAUSED');
});

function showToast(text) {
    const toast = document.createElement('div');
    toast.innerText = text;
    toast.style.cssText = `
        position: absolute; top: 20px; left: 50%; transform: translateX(-50%);
        background: rgba(40, 167, 69, 0.9); color: white; padding: 10px 20px;
        border-radius: 20px; font-weight: bold; z-index: 1000; transition: opacity 0.5s;
    `;
    document.body.appendChild(toast);
    setTimeout(() => {
        toast.style.opacity = '0';
        setTimeout(() => toast.remove(), 500);
    }, 3000);
}

document.getElementById('startPlay') || document.getElementById('startBtn').addEventListener('click', startStream);
document.getElementById('stopPlay') || document.getElementById('exitBtn').addEventListener('click', stopStream);
let videoPauseBtn = document.getElementById("videoPauseBtn");
if (!videoPauseBtn) {
    videoPauseBtn = document.createElement("a");
    videoPauseBtn.id = "videoPauseBtn";
    videoPauseBtn.className = "waves-effect waves-light btn-small orange";
    videoPauseBtn.textContent = "Pause Video";
    const stopBtn = document.getElementById("stopPlay");
    if (stopBtn && stopBtn.parentNode) stopBtn.parentNode.insertBefore(videoPauseBtn, stopBtn);
}
let audioPauseBtn = document.getElementById("audioPauseBtn");
if (!audioPauseBtn) {
    audioPauseBtn = document.createElement("a");
    audioPauseBtn.id = "audioPauseBtn";
    audioPauseBtn.className = "waves-effect waves-light btn-small orange";
    audioPauseBtn.textContent = "Pause Audio";
    const stopBtn = document.getElementById("stopPlay");
    if (stopBtn && stopBtn.parentNode) stopBtn.parentNode.insertBefore(audioPauseBtn, stopBtn);
}
videoPauseBtn.onclick = () => setMediaPaused('video', !videoPaused);
audioPauseBtn.onclick = () => setMediaPaused('audio', !audioPaused);
document.getElementById("fullscreenBtn").onclick = function() {
  var el = document.getElementById("video") || document.getElementById("player-container-id");
  if (el && el.requestFullscreen) { el.requestFullscreen(); }
  else if (el && el.webkitRequestFullscreen) { el.webkitRequestFullscreen(); }
};

layerSelect.addEventListener('change', (e) => {
    const val = e.target.value;
    if (val === "auto") {
        abrEngine.setAutoMode(true);
        if (videoPaused || abrEngine.currentTrackId === abrEngine.audioTrackId) {
            const lowestVideo = abrEngine.videoTrackIds[0];
            if (lowestVideo !== undefined) switchMediaTrack(lowestVideo, 'user_resume');
        }
        console.log(`[UI] Switched to AUTO mode`);
    } else {
        abrEngine.setAutoMode(false);
        const trackId = parseInt(val);
        if (!isNaN(trackId) && controlClient) {
            console.log(`[UI] Manual select track: ${trackId}`);
            // 通知引擎手动切换了，更新其内部状态
            abrEngine.notifyManualSwitch(trackId);
            switchMediaTrack(trackId, 'manual_user');
        }
    }
});

function startStream() {
    stopStream();
    const generation = ++readerGeneration;

    const url = urlInput.value.trim();
    if (!url) return alert('Please enter a WHEP URL');

    // Publish (WHIP) and read (WHEP) URLs differ by one letter and are easy
    // to swap by mistake. Sending a WHIP URL here silently registers this
    // recvonly connection as a publish session: no error is returned, but
    // no media ever flows and the control WebSocket is rejected as "not a
    // reader session" a moment later. Fail loudly instead.
    const lastSegment = url.split('?')[0].split('/').filter(Boolean).pop();
    if (lastSegment === 'whip') {
        const fixedUrl = url.replace(/\/whip(\?|$)/, '/whep$1');
        alert(`This is a WHIP (publish) URL, not a WHEP (read) URL.\nUse:\n${fixedUrl}`);
        return;
    }

    loadStatConfig().then(() => console.log('[StatAPI] ready for playback statistics'));

    statsContainer.innerHTML = '<div style="color: #00bcd4; text-align: center;">Connecting WHEP...</div>';
    previousTrackType = null;
    lastP2PDelayMs = null;
    lastP2PDelayAt = 0;
    lastP2PDelaySource = null;
    seiReaderAttached = false;

    reader = new MediaMTXWebRTCReader({
        url: url,
        maxBitrate: 2500, // 初始带宽限制
        onTrack: (evt) => {
            if (generation !== readerGeneration) return;
            if (evt.track.kind === 'video' || evt.track.kind === 'audio') {
                if (video.srcObject !== evt.streams[0]) {
                    video.srcObject = evt.streams[0];
                }
            }
            if (evt.track.kind === 'video' && !seiReaderAttached && evt.receiver &&
                typeof window.attachSeiTimestampReader === 'function') {
                seiReaderAttached = window.attachSeiTimestampReader(evt.receiver, (ts) => {
                    reportP2PDelay(ts, 'sei');
                });
                if (seiReaderAttached) console.log('[SEI] abs-timestamp reader attached');
            }
        },
        onError: (err) => {
            if (generation !== readerGeneration) return;
            console.error("Reader Error:", err);
            // A broken peer connection ends the current playback period.
            // A successful automatic reconnect will emit playing and start a
            // new period through statPlayStarted().
            statPlayEnded();
            statLagStartedAt = 0;
            statsContainer.innerHTML = `<div style="color: red; text-align: center;">Error: ${err}</div>`;
            // Force audio-only on connection failure to save bandwidth
            if (abrEngine && abrEngine.audioTrackId && abrEngine.currentTrackId !== abrEngine.audioTrackId) {
                console.warn("[Main] Connection lost, forcing audio-only mode");
                if (controlClient) switchMediaTrack(abrEngine.audioTrackId, 'connection_lost');
                pauseVideoForAudioOnly();
            }
        },
        onConnected: () => {
            if (generation !== readerGeneration || !reader) return;
            const sessionId = reader.sessionId;
            console.log(`[Glue] WHEP Connected. SessionID: ${sessionId}`);
            if (window.parent) window.parent.postMessage('mmxplayer-connected', '*');
            if (sessionId) {
                initControlClient(url, sessionId);
            }
        }
    });

    lastStats.timestamp = Date.now();
    statsInterval = setInterval(updateStats, 1000);
}

function initControlClient(whepUrl, sessionId) {
    // Destroy previous client to stop stale reconnects
    if (controlClient) {
        controlClient.close();
        controlClient = null;
    }
    controlClient = new MMXControlClient(whepUrl, sessionId, {
		onMediaState: updateMediaState,
        onConnected: () => {
            wsStatusDot.classList.remove('ws-disconnected');
            wsStatusDot.classList.add('ws-connected');
            layerSelect.disabled = false;
            layerSelect.innerHTML = '<option value="-1">Loading...</option>';
        },
        onDisconnected: () => {
            wsStatusDot.classList.remove('ws-connected');
            wsStatusDot.classList.add('ws-disconnected');
            layerSelect.disabled = true;
        },
        onTracksInfo: (tracks, activeId) => {
            console.log("[UI] Received Tracks Info:", tracks);
            
            // [关键修复] 获取当前视频实际宽度，传给 ABR 引擎用于探测真实初始状态
            const currentVideoWidth = video.videoWidth || 0;
            abrEngine.setTracks(tracks, activeId, currentVideoWidth);
            if (videoPaused && abrEngine.audioTrackId !== null) {
                abrEngine.currentTrackId = abrEngine.audioTrackId;
            } else if (activeId !== undefined && activeId !== null) {
                lastVideoTrackId = activeId;
            }
            
            if (!layerSelect.disabled){
                updateLayerSelectUI(tracks, activeId);
            }
        },
        onObsTimestamp: (data) => {
            // Fallback path (see comment above lastP2PDelayMs) - only used
            // when SEI reading isn't available. Only count frames from the
            // layer we're actually decoding: OBS tags each simulcast
            // layer's frames with that layer's own rid, and
            // frame_no/timestamp are meaningless if mismatched.
            const activeTrack = abrEngine.trackRegistry[abrEngine.currentTrackId];
            const activeRid = activeTrack ? String(activeTrack.rid) : null;
            if (activeRid === null || String(data.rid) !== activeRid) return;
            reportP2PDelay(data.timestamp, 'datachannel');
        },
        onLayerSwitched: (id) => {
            console.log("[UI] Received Track switch:", id);

            const currentTrack = abrEngine.trackRegistry[id];
            const wasAudioOnly = previousTrackType === 'audio';
            const isNowVideo = currentTrack && currentTrack.type === 'video';
            
            if (wasAudioOnly && isNowVideo) {
                console.log('[Main] Resuming from audio-only to video - forcing video play');
                handleVideoResume();
                resumeVideoFromAudioOnly();
            }
            
            previousTrackType = currentTrack ? currentTrack.type : null;
            abrEngine.notifyLayerSwitched(id);
            
            // Pause video on audio-only to save bandwidth
            if (id === abrEngine.audioTrackId) {
                pauseVideoForAudioOnly();
            }
            
            if (!abrEngine.isAutoMode && !layerSelect.disabled) {
                layerSelect.value = id;
            }
        }
    });
}

// ✅ NEW: Handle video resume after audio-only mode
function handleVideoResume() {
    if (!video || !video.srcObject) {
        console.warn('[Main] Cannot resume video - no video element or stream');
        return;
    }
    
    // Strategy 1: Ensure video is not paused
    if (video.paused) {
        console.log('[Main] Video is paused, attempting to play...');
        video.play().catch(err => {
            console.warn('[Main] Auto-play failed:', err);
        });
    }
    
    // Strategy 2: Force a small seek to trigger rendering
    // This helps in cases where the video element is "stuck"
    setTimeout(() => {
        if (video.readyState >= 2) { // HAVE_CURRENT_DATA or better
            const currentTime = video.currentTime;
            if (currentTime > 0.01) {
                video.currentTime = currentTime - 0.01;
                console.log('[Main] Applied micro-seek to trigger video rendering');
            }
        }
    }, 100);
    
    // Strategy 3: Force video element refresh
    // Some browsers need this to properly resume video rendering
    setTimeout(() => {
        if (video.paused && video.srcObject) {
            console.log('[Main] Video still paused after 500ms, forcing play');
            video.play().catch(err => {
                console.warn('[Main] Delayed auto-play failed:', err);
            });
        }
    }, 500);
}

function pauseVideoForAudioOnly() {
    if (video && !video.paused) {
        console.log('[Main] Pausing video for audio-only mode');
        video.pause();
    }
}

function resumeVideoFromAudioOnly() {
    if (video && video.paused && video.srcObject) {
        console.log('[Main] Resuming video from audio-only mode');
        video.play().catch(err => console.warn('[Main] Video resume failed:', err));
    }
}

function updateLayerSelectUI(tracks, activeId) {
    layerSelect.innerHTML = '';
    
    const autoOption = document.createElement('option');
    autoOption.value = 'auto';
    autoOption.text = 'Auto (ABR)';
    layerSelect.appendChild(autoOption);

    const videos = tracks.filter(t => t.type === 'video');
    // UI显示：码率从高到低
    videos.sort((a, b) => (b.bitrate || 0) - (a.bitrate || 0));

    videos.forEach(t => {
        const option = document.createElement('option');
        option.value = t.id;
        let label = t.label || `Video ${t.id}`;
        option.text = label;
        layerSelect.appendChild(option);
    });

    const audios = tracks.filter(t => t.type === 'audio');
    if (audios.length > 0) {
        const audioT = audios[0];
        const option = document.createElement('option');
        option.value = audioT.id;
        option.text = `Audio Opus`;
        option.style.fontWeight = 'bold';
        option.style.color = '#ff9800'; 
        layerSelect.appendChild(option);
    }

    if (activeId !== undefined && activeId !== null) {
        if (!abrEngine.isAutoMode) {
            layerSelect.value = activeId;
        } else {
            layerSelect.value = 'auto';
        }
    }
}

function stopStream() {
    statPlayEnded();
    statLagStartedAt = 0;
    if (controlClient) {
        controlClient.close();
        controlClient = null;
    }
    if (reader) {
        reader.close();
        reader = null;
    }
    if (statsInterval) {
        clearInterval(statsInterval);
        statsInterval = null;
    }
    video.srcObject = null;
    statsContainer.innerHTML = '<div style="color: #888; text-align: center;">Stopped</div>';
    
    wsStatusDot.classList.remove('ws-connected');
    wsStatusDot.classList.remove('ws-disconnected');
    layerSelect.disabled = true;
    layerSelect.innerHTML = '<option value="-1">Auto</option>';
    
    abrEngine.setAutoMode(true);
    previousTrackType = null;
}

async function updateStats() {
    if (!reader || !reader.pc) return;
    const pc = reader.pc;
    if (pc.connectionState !== 'connected' && pc.connectionState !== 'checking') return;

    try {
        const stats = await pc.getStats();
        const now = Date.now();
        const deltaTime = (now - lastStats.timestamp) / 1000;
        if (deltaTime <= 0) return;

        let videoStats = null;
        let audioStats = null;
        let networkStats = null;
        // [新增] 用于查找 codec 名称
        const codecs = new Map(); 

        stats.forEach(report => {
            if (report.type === 'inbound-rtp' && report.kind === 'video') videoStats = report;
            if (report.type === 'inbound-rtp' && report.kind === 'audio') audioStats = report;
            if (report.type === 'candidate-pair' && report.state === 'succeeded') networkStats = report;
            // [新增] 收集 codec 信息
            if (report.type === 'codec') {
                codecs.set(report.id, report.mimeType); // e.g. "video/H264"
            }
        });

        // --- 计算实时指标 ---
        let videoKbps = 0;
        let audioKbps = 0;
        let fps = 0;
        let currentPacketLoss = 0;

        if (videoStats) {
            videoKbps = ((videoStats.bytesReceived - lastStats.videoBytes) * 8 / deltaTime / 1000);
            fps = videoStats.framesPerSecond || 0;
            const vLoss = (videoStats.packetsLost || 0) - (lastStats.videoPacketsLost || 0);
            if (vLoss > 0) currentPacketLoss += vLoss;
            lastStats.videoBytes = videoStats.bytesReceived;
            lastStats.videoPacketsLost = videoStats.packetsLost || 0;
        }

        if (audioStats) {
            audioKbps = ((audioStats.bytesReceived - lastStats.audioBytes) * 8 / deltaTime / 1000);
            const aLoss = (audioStats.packetsLost || 0) - (lastStats.audioPacketsLost || 0);
            if (aLoss > 0) currentPacketLoss += aLoss;
            lastStats.audioBytes = audioStats.bytesReceived;
            lastStats.audioPacketsLost = audioStats.packetsLost || 0;
        }

        lastStats.timestamp = now;

        // --- 调用 ABR 引擎 ---
        if (abrEngine) {
            abrEngine.update(videoKbps, audioKbps, fps, currentPacketLoss);
        }

        // RTT/jitter-buffer-based rough p2p delay estimate. Used both as
        // the LATENCY_REPORT payload's estimated_e2e_ms and, when the
        // SEI/DataChannel measurement looks clock-skewed (see
        // P2P_DELAY_CLOCK_SUSPECT_MS), as the displayed P2P Delay fallback.
        const rttMs = networkStats?.currentRoundTripTime ? networkStats.currentRoundTripTime * 1000 : 0;
        const jitterBufferMs = videoStats?.jitterBufferDelay && videoStats?.jitterBufferEmittedCount
            ? (videoStats.jitterBufferDelay / videoStats.jitterBufferEmittedCount) * 1000
            : 0;
        const estimatedP2PDelayMs = rttMs / 2 + jitterBufferMs + 10;

        // --- 渲染 UI ---
        let html = '';

        if (videoStats) {
            const displayW = video.videoWidth || videoStats.frameWidth || 0;
            const displayH = video.videoHeight || videoStats.frameHeight || 0;
            
            // [新增] 获取 Video Codec 名称
            let vCodec = 'N/A';
            if (videoStats.codecId && codecs.has(videoStats.codecId)) {
                // mimeType 格式通常为 "video/H264"，我们只取后半部分
                vCodec = codecs.get(videoStats.codecId).split('/')[1] || 'Unknown';
            }

            html += renderStatGroup('Video', {
                'Codec': vCodec, // [显示]
                'Resolution': `${displayW}x${displayH}`,
                'Recv Bitrate': `${videoKbps.toFixed(0)} kbps`,
                'FPS': fps.toFixed(1),
                'Packet Loss': `${videoStats.packetsLost} pkts`
            });
        }

        if (audioStats) {
            // [新增] 获取 Audio Codec 名称
            let aCodec = 'N/A';
            if (audioStats.codecId && codecs.has(audioStats.codecId)) {
                aCodec = codecs.get(audioStats.codecId).split('/')[1] || 'Unknown';
            }

            html += renderStatGroup('Audio', {
                'Codec': aCodec, // [显示]
                'Recv Bitrate': `${audioKbps.toFixed(0)} kbps`,
                'Packet Loss': `${audioStats.packetsLost} pkts`,
                'Jitter': `${(audioStats.jitter * 1000).toFixed(1)} ms`
            });
        }

        if (networkStats) {
            let bw = 'N/A';
            if (networkStats.availableIncomingBitrate) {
                bw = `${(networkStats.availableIncomingBitrate / 1000).toFixed(0)} kbps`;
            }
            
            let abrStatus = (abrEngine && abrEngine.isAutoMode) ? 'Auto' : 'Manual';
            if (abrEngine && abrEngine.abrCooldown > 0) abrStatus += ` (Cool ${abrEngine.abrCooldown})`;

            const p2pFresh = lastP2PDelayMs !== null && (Date.now() - lastP2PDelayAt) < P2P_DELAY_STALE_MS;
            // A fresh measurement under P2P_DELAY_CLOCK_SUSPECT_MS almost
            // certainly means the player's local clock isn't NTP-synced
            // (see const comment above) rather than a real sub-100ms delay
            // - fall back to the RTT/jitter-buffer estimate instead of
            // showing a misleadingly tiny or negative number.
            const p2pLabel = !p2pFresh
                ? 'N/A'
                : lastP2PDelayMs < P2P_DELAY_CLOCK_SUSPECT_MS
                    ? `~${estimatedP2PDelayMs.toFixed(0)} ms (est.)`
                    : `${lastP2PDelayMs.toFixed(0)} ms (${lastP2PDelaySource === 'sei' ? 'SEI' : 'DC'})`;

            html += renderStatGroup('Network', {
                'RTT': `${(networkStats.currentRoundTripTime * 1000).toFixed(1)} ms`,
                'Est. Bandwidth': bw,
                'ABR State': abrStatus,
                'P2P Delay': p2pLabel
            });
        }

        if (html) statsContainer.innerHTML = html;

        // ── LATENCY_REPORT: send e2e metrics to server every second ──
        if (controlClient && controlClient.ws && controlClient.ws.readyState === WebSocket.OPEN) {
            const loss = currentPacketLoss;
            const fpsVal = videoStats?.framesPerSecond || 0;

            controlClient.sendLatencyReport({
                rtt_ms: rttMs,
                jitter_buffer_ms: jitterBufferMs,
                packets_lost: loss,
                fps: fpsVal,
                estimated_e2e_ms: estimatedP2PDelayMs
            });
        }

    } catch (e) {
        console.warn("Error updating stats:", e);
    }
}

function renderStatGroup(title, data) {
    let rows = '';
    for (const [key, value] of Object.entries(data)) {
        let valClass = '';
        if (key === 'Packet Loss' && parseInt(value) > 0) valClass = 'warn';
        if (key === 'Est. Bandwidth') valClass = 'good';
        rows += `<div class="stat-row"><span class="stat-key">${key}:</span><span class="stat-val ${valClass}">${value}</span></div>`;
    }
    return `<div class="stat-group"><div class="stat-title">${title}</div>${rows}</div>`;
}

let videoPaused = false;
let audioPaused = false;

function setMediaPaused(kind, paused) {
    if (!reader || !controlClient) return;
    controlClient.setMediaState({ [kind]: paused ? 'paused' : 'resumed' });
}

function updateMediaState(state) {
    videoPaused = state.video === 'paused';
    audioPaused = state.audio === 'paused';
    videoPauseBtn.innerText = videoPaused ? 'Resume Video' : 'Pause Video';
    audioPauseBtn.innerText = audioPaused ? 'Resume Audio' : 'Pause Audio';
    video.style.opacity = videoPaused ? '0.5' : '1';
    if (videoPaused && abrEngine.audioTrackId !== null) {
        if (abrEngine.videoTrackIds.includes(abrEngine.currentTrackId)) lastVideoTrackId = abrEngine.currentTrackId;
        abrEngine.currentTrackId = abrEngine.audioTrackId;
        if (!abrEngine.isAutoMode) layerSelect.value = abrEngine.audioTrackId;
    } else if (abrEngine.currentTrackId === abrEngine.audioTrackId && lastVideoTrackId !== null) {
        abrEngine.currentTrackId = lastVideoTrackId;
    }
    const audioOnly = abrEngine.currentTrackId === abrEngine.audioTrackId;
    layerSelect.disabled = videoPaused && !audioOnly;
}
