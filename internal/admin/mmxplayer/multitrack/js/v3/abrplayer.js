const CONFIG = {
  DEBUG: true,
  SCRIPT_VERSION: 'v1.0.3-audio360',
  ABR_SWITCH_COOLDOWN_MS: 50000,
  MONITOR_INTERVAL_MS: 2000,
  UPGRADE_PROBE_INTERVAL_MS: 12000,
  BANDWIDTH_LOG_INTERVAL_MS: 60000,
  ABR_PROBE_LOG_INTERVAL_MS: 60000,
  AUDIO_DEBUG_LOG_INTERVAL_MS: 60000,
  LOG_NORMAL_AUDIO_STATS: false,
  ZERO_BANDWIDTH_PROBE_INTERVAL_MS: 60000,
  AUDIO_ONLY_RECONNECT_DELAY_MS: 8000,
  AUDIO_ONLY_RECONNECT_MAX_ATTEMPTS: 2,
  UPGRADE_CONFIRM_COUNT: 1,
  DOWNGRADE_CONFIRM_COUNT: 3,
  SEVERE_DOWNGRADE_CONFIRM_COUNT: 1,
  UPGRADE_PROBATION_MS: 15000,
  UPGRADE_BACKOFF_MS: 90000,
  NETWORK_ERROR_GRACE_MS: 60000,
  BITRATE_DOWN_FACTOR: 0.6,
  STALL_THRESHOLD_MS: 5000,
  // Bandwidth probing downloads static same-origin assets only. It is not tied
  // to H.264/HEVC codec choice; codec handling is done by playback errors.
  BANDWIDTH_TEST_URLS: [
    'NIOES8001.jpeg'
  ],
  DEFAULT_FALLBACK_STREAM: '3drush-fwh_audio'
};

const UPGRADE_THRESHOLDS_KBPS = {
  bottom: 300,
  economic: 600,
  standard: 1500
};

const DOWNGRADE_THRESHOLDS_KBPS = {
  bottom: 100,
  economic: 250,
  standard: 500,
  high: 1200
};

const QUALITY_ORDER = ['bottom', 'economic', 'standard', 'high'];

console.log(`[ABR] script loaded: ${CONFIG.SCRIPT_VERSION}`);

let currentSiteName = '3drush-fwv';
let STREAM_LADDER = buildStreamLadder(currentSiteName);

function buildStreamLadder(siteName) {
    const ladder = [
        { level: 0, key: 'bottom',   name: `${siteName}_audio`,    bitrate: 128,  codec: 'opus', online: true },
        { level: 1, key: 'economic', name: `${siteName}_economic`, bitrate: 400,  codec: 'h264', online: true },
        { level: 2, key: 'standard', name: `${siteName}_standard`, bitrate: 1000, codec: 'h264', online: true },
        { level: 3, key: 'high',     name: siteName,               bitrate: 2000, codec: 'h264', online: true }
    ];
    return ladder.filter(stream => stream.online);
}

function setSiteName(siteName) {
    siteName = (siteName || '').trim();
    if (!siteName) return;
    currentSiteName = siteName;
    STREAM_LADDER = buildStreamLadder(siteName);
    CONFIG.DEFAULT_FALLBACK_STREAM = `${siteName}_audio`;
}

function setStreamLadder(siteName, streamNames) {
    siteName = (siteName || '').trim();
    if (!siteName) return;
    currentSiteName = siteName;
    STREAM_LADDER = buildStreamLadder(siteName).map(profile => {
        let name = streamNames && streamNames[profile.key];
        return Object.assign({}, profile, { name: (name || profile.name).trim() });
    });
    const bottom = STREAM_LADDER.find(s => s.key === 'bottom');
    CONFIG.DEFAULT_FALLBACK_STREAM = bottom ? bottom.name : `${siteName}_audio`;
}

function getAudioOnlyPlaybackStreamName(logicalStreamName) {
    const economic = getPreferredStreamForQuality('economic', { preferHevc: false });
    return economic || logicalStreamName;
}

function getPlaybackStreamForRequest(streamName) {
    if (isAudioOnlyStream(streamName, '')) return getAudioOnlyPlaybackStreamName(streamName);
    const profile = getStreamProfile(streamName);
    if (!profile || streamName.endsWith('_q0') || streamName.endsWith('_q1') || streamName.endsWith('_q2')) return streamName;
    const layer = { high: 'q0', standard: 'q1', economic: 'q2' }[profile.key];
    return layer ? `${streamName.replace(/_(high|standard|economic)$/, '')}_${layer}` : streamName;
}

let tcplayer = null;
let autoSelectActive = false;
let currentWebrtcUrl = null;
let monitorTimer = null;
let playTimer = null;
let startPlayTime = 0;
let startPlayTimeLast = -1;
let playerMuted = getCookie('phil_player_muted') === '1';
let lastAudioDebugLogTime = 0;
let audioOnlyReconnectTimer = null;
let audioOnlyReconnectAttempts = 0;
let localAudioOnlyMode = false;
let localAudioOnlySourceStream = null;

const ABRState = {
  currentStream: null,
  lastSwitchTime: 0,
  currentReceiveKbps: 0,
  probedBandwidthKbps: 0,
  inCooldown: false,
  isSwitching: false,
  consecutiveLowCount: 0,
  consecutiveProbeLowCount: 0,
  consecutiveProbeLowTarget: null,
  consecutiveHighCount: 0,
  lastProbeTime: 0,
  lastBandwidthLogTime: 0,
  lastAbrProbeLogTime: 0,
  lastNetworkIssueLogTime: 0,
  lastNetworkIssueTime: 0,
  zeroBandwidthBackoffUntil: 0,
  probeInFlight: false,
  lastStallTime: 0,
  lastErrorTime: 0,
  lastErrorCode: null,
  lastUpgradeTime: 0,
  lastUpgradeFromStream: null,
  upgradeBackoffUntil: 0,
  hevcBackoffUntil: 0
};

function getStreamProfile(streamName) {
  if (!streamName) return STREAM_LADDER[0]; 
  return STREAM_LADDER.find(s => s.name === streamName);
}

function getCurrentStreamIndex() {
    return STREAM_LADDER.findIndex(s => s.name === ABRState.currentStream);
}

function getQualityKey(profileOrKey) {
    const key = typeof profileOrKey === 'string' ? profileOrKey : profileOrKey && profileOrKey.key;
    return key;
}

function getQualityRank(profileOrKey) {
    return QUALITY_ORDER.indexOf(getQualityKey(profileOrKey));
}

function getPreferredStreamForQuality(qualityKey, options = {}) {
    if (qualityKey === 'standard') {
        const h264 = STREAM_LADDER.find(s => s.key === 'standard' && s.online);
        if (h264) return h264.name;
    }

    const stream = STREAM_LADDER.find(s => getQualityKey(s) === qualityKey && s.online);
    return stream ? stream.name : null;
}

function getPreviousQualityStreamName(profileOrIndex) {
    const profile = typeof profileOrIndex === 'number' ? STREAM_LADDER[profileOrIndex] : profileOrIndex;
    const rank = getQualityRank(profile);
    for (let i = rank - 1; i >= 0; i--) {
        const stream = getPreferredStreamForQuality(QUALITY_ORDER[i]);
        if (stream) return stream;
    }
    return CONFIG.DEFAULT_FALLBACK_STREAM;
}

function getNextQualityStreamName(profileOrIndex, options = {}) {
    const profile = typeof profileOrIndex === 'number' ? STREAM_LADDER[profileOrIndex] : profileOrIndex;
    const rank = getQualityRank(profile);
    for (let i = rank + 1; i < QUALITY_ORDER.length; i++) {
        const stream = getPreferredStreamForQuality(QUALITY_ORDER[i], options);
        if (stream) return stream;
    }
    return null;
}

function getPreferredStandardProfile() {
    return STREAM_LADDER.find(s => s.key === 'standard');
}

function markNetworkIssue(reason) {
    ABRState.lastNetworkIssueTime = Date.now();
    const now = Date.now();
    if (now - ABRState.lastNetworkIssueLogTime >= CONFIG.ABR_PROBE_LOG_INTERVAL_MS) {
        ABRState.lastNetworkIssueLogTime = now;
        console.warn(`[ABR] Network issue suspected: ${reason}`);
    }
}

function isNetworkSuspectForHevcFailure() {
    const now = Date.now();
    if (typeof navigator !== 'undefined' && navigator.onLine === false) return true;
    if (ABRState.lastNetworkIssueTime && now - ABRState.lastNetworkIssueTime < CONFIG.NETWORK_ERROR_GRACE_MS) return true;
    if (ABRState.lastStallTime && now - ABRState.lastStallTime < CONFIG.NETWORK_ERROR_GRACE_MS) return true;
    if (ABRState.probedBandwidthKbps > 0 && ABRState.probedBandwidthKbps < UPGRADE_THRESHOLDS_KBPS.standard) return true;
    return false;
}

function fallbackUnderscoreToLegacy(reason) {
    const current = ABRState.currentStream || '';
    if (!current.includes('_') || typeof legacyStreamName !== 'function') return false;

    const legacy = legacyStreamName(current);
    if (!legacy || legacy === current) return false;

    if (typeof applyLegacyStreamNameMode === 'function') {
        applyLegacyStreamNameMode();
    } else {
        window.__useLegacyStreamNames = true;
    }
    console.warn(`[ABR] Legacy stream-name fallback. Reason: ${reason}. ${current} -> ${legacy}`);
    ABRState.isSwitching = true;
    safeSwitchToStream(legacy, 'legacy_fallback');
    return true;
}

function checkCooldown() {
    const now = Date.now();
    if (now - ABRState.lastSwitchTime < CONFIG.ABR_SWITCH_COOLDOWN_MS) {
        return true;
    }
    return false;
}

function selectBestStreamBelow(kbps) {
    return selectBestStreamByBitrate(kbps);
}

function selectBestStreamByBitrate(kbps) {
    let key = 'high';
    if (kbps < DOWNGRADE_THRESHOLDS_KBPS.economic) key = 'bottom';
    else if (kbps < DOWNGRADE_THRESHOLDS_KBPS.standard) key = 'economic';
    else if (kbps < DOWNGRADE_THRESHOLDS_KBPS.high) key = 'standard';

    return getPreferredStreamForQuality(key) || CONFIG.DEFAULT_FALLBACK_STREAM;
}

function selectDowngradeStreamForCurrent(kbps) {
    const currentIndex = getCurrentStreamIndex();
    if (currentIndex <= 0) return null;

    const currentProfile = STREAM_LADDER[currentIndex];
    const currentRank = getQualityRank(currentProfile);
    const threshold = DOWNGRADE_THRESHOLDS_KBPS[getQualityKey(currentProfile)];
    const shouldDowngrade = threshold && kbps < threshold;

    if (!shouldDowngrade) return null;

    const measuredTarget = selectBestStreamByBitrate(kbps);
    const measuredProfile = getStreamProfile(measuredTarget);
    const measuredRank = getQualityRank(measuredProfile);
    if (measuredTarget && measuredRank >= 0 && measuredRank < currentRank) return measuredTarget;

    return getPreviousQualityStreamName(currentProfile);
}

function shouldRollbackUpgrade() {
    return ABRState.lastUpgradeFromStream
        && Date.now() - ABRState.lastUpgradeTime <= CONFIG.UPGRADE_PROBATION_MS;
}

function clearUpgradeProbeState() {
    ABRState.consecutiveHighCount = 0;
    ABRState.consecutiveProbeLowCount = 0;
    ABRState.consecutiveProbeLowTarget = null;
    ABRState.probedBandwidthKbps = 0;
}

function resetAbrCounters() {
    ABRState.consecutiveLowCount = 0;
    ABRState.consecutiveProbeLowCount = 0;
    ABRState.consecutiveProbeLowTarget = null;
    ABRState.consecutiveHighCount = 0;
}

function isAudioOnlyStream(streamName, source) {
    const profile = getStreamProfile(streamName);
    if (profile && profile.key === 'bottom') return true;
    const value = `${streamName || ''} ${source || ''}`.toLowerCase();
    return value.includes('_audio') || value.includes('audio');
}

function shouldStoreFallbackSnapshot(streamName) {
    const profile = getStreamProfile(streamName);
    const quality = getQualityKey(profile);
    return quality === 'high' || quality === 'standard';
}

function describePlaybackStream(width, height) {
    const profile = getStreamProfile(ABRState.currentStream) || {};
    const codec = (profile.codec || (isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl) ? 'opus' : 'unknown')).toUpperCase();
    const quality = getQualityKey(profile) || 'unknown';
    const level = getQualityRank(profile);
    const audioOnly = isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
    const resolution = !audioOnly && width > 0 && height > 0 ? `${width}x${height}` : 'audio-only';
    return {
        stream: ABRState.currentStream || 'unknown',
        quality,
        level: level >= 0 ? level : 'unknown',
        codec,
        resolution,
        source: localAudioOnlySourceStream || ABRState.currentStream || 'unknown'
    };
}

function setVideoPlaybackVisible(visible) {
    const el = document.getElementById('player-container-id');
    if (!el) return;
    el.style.display = visible ? '' : 'none';
}

function setLocalVideoTracksEnabled(enabled) {
    const el = document.getElementById('player-container-id');
    if (!el || !el.srcObject || typeof el.srcObject.getVideoTracks !== 'function') return;
    try {
        el.srcObject.getVideoTracks().forEach(track => {
            track.enabled = !!enabled;
        });
    } catch (e) {}
}

function enterLocalAudioOnlyMode(audioStreamName, reason) {
    if (!tcplayer || isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl)) {
        return false;
    }

    localAudioOnlyMode = true;
    localAudioOnlySourceStream = ABRState.currentStream;
    clearAudioOnlyReconnectTimer();
    audioOnlyReconnectAttempts = 0;

    freezeLastFrame();
    setLocalVideoTracksEnabled(false);
    setVideoPlaybackVisible(false);
    if (typeof renderAudioFallbackSnapshot === 'function') {
        renderAudioFallbackSnapshot(audioStreamName);
    }

    console.warn(`[ABR] Enter local audio-only mode. Reason: ${reason}. logical=${audioStreamName}, keptSource=${localAudioOnlySourceStream}`);
    ABRState.currentStream = audioStreamName;
    ABRState.lastSwitchTime = Date.now();
    clearUpgradeProbeState();
    setCookie('phil_stream_choice', audioStreamName, 1);
    ABRState.isSwitching = false;
    return true;
}

function exitLocalAudioOnlyMode() {
    if (!localAudioOnlyMode) return;
    console.log(`[ABR] Exit local audio-only mode. previousSource=${localAudioOnlySourceStream || '-'}`);
    localAudioOnlyMode = false;
    localAudioOnlySourceStream = null;
    setLocalVideoTracksEnabled(true);
    setVideoPlaybackVisible(true);
}

function isFatalPlayerError(errCode) {
    return [14, 1001, 1002, -2001, -2004, -2005].includes(Number(errCode));
}

function cleanupPlayerDom() {
    const container = document.getElementById('local-video');
    if (!container) return;

    container.querySelectorAll(
        '#player-container-id, .video-js, .vjs-error-display, .vjs-modal-dialog, video, #snapshot-canvas'
    ).forEach(el => {
        if (!el.classList || !el.classList.contains('stat-info')) {
            el.remove();
        }
    });
}

function clearPlayerErrorState() {
    try {
        if (tcplayer && typeof tcplayer.error === 'function') {
            tcplayer.error(null);
        }
    } catch (e) {}

    document.querySelectorAll('.vjs-error, .vjs-error-display, .vjs-modal-dialog')
        .forEach(el => {
            if (el.classList) el.classList.remove('vjs-error');
            if (el.classList && (el.classList.contains('vjs-error-display') || el.classList.contains('vjs-modal-dialog'))) {
                el.remove();
            }
        });
}

function clearAudioOnlyReconnectTimer() {
    if (audioOnlyReconnectTimer) {
        clearTimeout(audioOnlyReconnectTimer);
        audioOnlyReconnectTimer = null;
    }
}

function scheduleAudioOnlyReconnect(reason) {
    if (audioOnlyReconnectTimer) return;
    const stream = ABRState.currentStream;
    if (!stream || !isAudioOnlyStream(stream, currentWebrtcUrl)) return;
    if (audioOnlyReconnectAttempts >= CONFIG.AUDIO_ONLY_RECONNECT_MAX_ATTEMPTS) {
        console.warn(`[ABR] Audio-only reconnect skipped after ${audioOnlyReconnectAttempts} failed attempts. Waiting for bandwidth recovery.`);
        return;
    }

    console.warn(`[ABR] Audio-only reconnect scheduled in ${CONFIG.AUDIO_ONLY_RECONNECT_DELAY_MS}ms. Reason: ${reason}`);
    audioOnlyReconnectTimer = setTimeout(async () => {
        audioOnlyReconnectTimer = null;
        if (ABRState.currentStream !== stream || !isAudioOnlyStream(stream, currentWebrtcUrl)) return;
        if (ABRState.isSwitching) return;

        audioOnlyReconnectAttempts++;
        console.warn(`[ABR] Reconnecting audio-only stream after player error. stream=${stream}`);
        ABRState.isSwitching = true;
        await safeSwitchToStream(stream, 'audio_retry');
    }, CONFIG.AUDIO_ONLY_RECONNECT_DELAY_MS);
}

function maybeDowngradeByBandwidth(kbps, source) {
    const currentIndex = getCurrentStreamIndex();
    if (currentIndex <= 0) {
        ABRState.consecutiveProbeLowCount = 0;
        return false;
    }

    const currentProfile = STREAM_LADDER[currentIndex];
    const currentQuality = getQualityKey(currentProfile);
    const threshold = DOWNGRADE_THRESHOLDS_KBPS[currentQuality];
    const targetStream = selectDowngradeStreamForCurrent(kbps);
    const targetIndex = STREAM_LADDER.findIndex(s => s.name === targetStream);
    if (targetIndex >= 0 && targetIndex < currentIndex) {
        const currentRank = getQualityRank(STREAM_LADDER[currentIndex]);
        const targetRank = getQualityRank(STREAM_LADDER[targetIndex]);
        const confirmCount = targetRank >= 0 && targetRank <= currentRank - 2
            ? CONFIG.SEVERE_DOWNGRADE_CONFIRM_COUNT
            : CONFIG.DOWNGRADE_CONFIRM_COUNT;
        if (ABRState.consecutiveProbeLowTarget !== targetStream) {
            ABRState.consecutiveProbeLowCount = 0;
            ABRState.consecutiveProbeLowTarget = targetStream;
        }
        ABRState.consecutiveProbeLowCount++;
        ABRState.consecutiveHighCount = 0;
        markNetworkIssue(`${source} low: ${kbps} kbps < ${threshold} kbps threshold for ${currentQuality}`);
        logAbrProbeProgress(`[ABR] Downgrade probe low x${ABRState.consecutiveProbeLowCount}/${confirmCount}: ${kbps} kbps < ${threshold} kbps threshold (${currentQuality}) via ${source} -> ${targetStream}`);
        if (ABRState.consecutiveProbeLowCount >= confirmCount) {
            performDowngrade(`${source} low: ${kbps} kbps < ${threshold} kbps threshold (${currentQuality}) -> ${targetStream}`, false, kbps);
            ABRState.consecutiveProbeLowCount = 0;
            ABRState.consecutiveProbeLowTarget = null;
        }
        return true;
    }

    ABRState.consecutiveProbeLowCount = 0;
    ABRState.consecutiveProbeLowTarget = null;
    return false;
}

async function probeAvailableBandwidth() {
    const urls = resolveBandwidthTestUrls();

    for (const baseUrl of urls) {
        const separator = baseUrl.includes('?') ? '&' : '?';
        const probeUrl = `${baseUrl}${separator}_probe=${Date.now()}`;
        const rawKbps = await probeBandwidth(probeUrl);
        const kbps = rawKbps === null ? null : parseFloat(rawKbps);
        if (kbps > 0) {
            ABRState.probedBandwidthKbps = kbps;
            ABRState.lastProbeTime = Date.now();
            ABRState.zeroBandwidthBackoffUntil = 0;
            logBandwidthProbeResult(kbps, baseUrl);
            return kbps;
        }
        console.warn(`[BW] probe source unavailable, trying fallback if configured: ${baseUrl}`);
    }

    ABRState.lastProbeTime = Date.now();
    markNetworkIssue('bandwidth probe unavailable');
    console.warn('[BW] All bandwidth probe sources unavailable; keep current stream.');
    return null;
}

function getPlayerVolume() {
    return playerMuted ? 0 : 1;
}

function updateMuteButton() {
    const icon = document.querySelector('#mutePlay i');
    if (!icon) return;
    icon.textContent = playerMuted ? 'volume_off' : 'volume_up';
}

function applyPlayerAudioSettings() {
    if (!tcplayer) return;
    try {
        if (typeof tcplayer.muted === 'function') {
            tcplayer.muted(playerMuted);
        }
    } catch (e) {}
    try {
        if (typeof tcplayer.volume === 'function') {
            tcplayer.volume(getPlayerVolume());
        }
    } catch (e) {}
    updateMuteButton();
}

function togglePlayerMuted() {
    playerMuted = !playerMuted;
    setCookie('phil_player_muted', playerMuted ? '1' : '0', 30);
    applyPlayerAudioSettings();
    console.log(`[PlayerAudio] user ${playerMuted ? 'muted' : 'unmuted'} player, volume=${getPlayerVolume()}`);
}

function setPlayerMuted(muted, options = {}) {
    playerMuted = !!muted;
    if (options.persist) {
        setCookie('phil_player_muted', playerMuted ? '1' : '0', 30);
    }
    applyPlayerAudioSettings();
    console.log(`[PlayerAudio] ${playerMuted ? 'muted' : 'unmuted'} player by option, volume=${getPlayerVolume()}`);
}

function compactAudioStats(audio) {
    if (!audio || Object.keys(audio).length === 0) return 'none';
    const parts = [];
    ['bitrate', 'bytesReceived', 'packetsReceived', 'packetsLost', 'jitter', 'sampleRate', 'audioLevel'].forEach(key => {
        if (audio[key] !== undefined && audio[key] !== null && audio[key] !== '') {
            parts.push(`${key}=${audio[key]}`);
        }
    });
    return parts.length ? parts.join(', ') : JSON.stringify(audio);
}

function hasActiveAudioStats(audio) {
    if (!audio || Object.keys(audio).length === 0) return false;
    const bitrate = Number.isFinite(Number(audio.bitrate)) ? Number(audio.bitrate) : 0;
    const packetsReceived = Number.isFinite(Number(audio.packetsReceived)) ? Number(audio.packetsReceived) : 0;
    const bytesReceived = Number.isFinite(Number(audio.bytesReceived)) ? Number(audio.bytesReceived) : 0;
    const audioLevel = Number.isFinite(Number(audio.audioLevel)) ? Number(audio.audioLevel) : 0;
    return bitrate > 0 || packetsReceived > 0 || bytesReceived > 0 || audioLevel > 0;
}

function logAudioStats(data) {
    const now = Date.now();
    if (now - lastAudioDebugLogTime < CONFIG.AUDIO_DEBUG_LOG_INTERVAL_MS) return;
    lastAudioDebugLogTime = now;
    const audio = data && data.audio;
    const hasAudioStats = !!(audio && Object.keys(audio).length > 0);
    const bitrate = audio && Number.isFinite(Number(audio.bitrate)) ? Math.round(Number(audio.bitrate) / 1000) : 0;
    const packetsReceived = audio && Number.isFinite(Number(audio.packetsReceived)) ? Number(audio.packetsReceived) : 0;
    const hasAudioSignal = hasAudioStats && (bitrate > 0 || packetsReceived > 0 || Number(audio.audioLevel) > 0);
    if (!hasAudioStats) {
        console.warn(`[PlayerAudio] no active audio stats from Tencent player. stream=${ABRState.currentStream}, muted=${playerMuted}, volume=${getPlayerVolume()}, stats=${compactAudioStats(audio)}`);
        return;
    }
    if (bitrate <= 0 && hasAudioSignal) {
        if (CONFIG.LOG_NORMAL_AUDIO_STATS) {
            console.log(`[PlayerAudio] stream=${ABRState.currentStream}, muted=${playerMuted}, volume=${getPlayerVolume()}, bitrate=pending, stats=${compactAudioStats(audio)}`);
        }
        return;
    }
    if (CONFIG.LOG_NORMAL_AUDIO_STATS) {
        console.log(`[PlayerAudio] stream=${ABRState.currentStream}, muted=${playerMuted}, volume=${getPlayerVolume()}, bitrate=${bitrate} kbps, stats=${compactAudioStats(audio)}`);
    }
}

function logBandwidthProbeResult(kbps, baseUrl) {
    const now = Date.now();
    if (now - ABRState.lastBandwidthLogTime < CONFIG.BANDWIDTH_LOG_INTERVAL_MS) return;
    ABRState.lastBandwidthLogTime = now;
    console.log(`[BW] Available bandwidth probe result: ${kbps} kbps`);
}

function logAbrProbeProgress(message) {
    const now = Date.now();
    if (now - ABRState.lastAbrProbeLogTime < CONFIG.ABR_PROBE_LOG_INTERVAL_MS) return;
    ABRState.lastAbrProbeLogTime = now;
    console.log(message);
}

function resolveBandwidthTestUrls() {
    const configured = Array.isArray(CONFIG.BANDWIDTH_TEST_URLS)
        ? CONFIG.BANDWIDTH_TEST_URLS
        : [CONFIG.BANDWIDTH_TEST_URL].filter(Boolean);
    const urls = [];
    const add = value => {
        if (!value) return;
        try {
            const resolved = new URL(value, window.location.href).href;
            if (!urls.includes(resolved)) urls.push(resolved);
        } catch (e) {}
    };

    configured.forEach(add);
    return urls;
}

function playbackHealthyForUpgrade() {
    const now = Date.now();
    const audioOnly = isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
    if (now < ABRState.upgradeBackoffUntil) return false;
    if (!audioOnly && ABRState.lastStallTime && now - ABRState.lastStallTime < CONFIG.UPGRADE_PROBATION_MS) return false;
    if (!audioOnly && ABRState.lastErrorTime && now - ABRState.lastErrorTime < CONFIG.UPGRADE_PROBATION_MS) return false;
    return true;
}

async function maybeProbeAndUpgrade() {
    if (!autoSelectActive || ABRState.isSwitching || ABRState.probeInFlight) return;
    if (!playbackHealthyForUpgrade()) {
        ABRState.consecutiveHighCount = 0;
        return;
    }

    const currentIndex = getCurrentStreamIndex();
    if (currentIndex < 0) {
        ABRState.consecutiveHighCount = 0;
        return;
    }

    const now = Date.now();
    if (now < ABRState.zeroBandwidthBackoffUntil) {
        console.log(`[BW] Skip probe after 0 kbps result until ${new Date(ABRState.zeroBandwidthBackoffUntil).toISOString()}`);
        return;
    }
    if (now - ABRState.lastProbeTime < CONFIG.UPGRADE_PROBE_INTERVAL_MS) return;

    let kbps = null;
    ABRState.probeInFlight = true;
    try {
        kbps = await probeAvailableBandwidth();
    } finally {
        ABRState.probeInFlight = false;
    }
    if (kbps === null || kbps <= 0) return;

    const audioOnly = isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
    if (!audioOnly && checkCooldown()) {
        resetAbrCounters();
        return;
    }

    if (maybeDowngradeByBandwidth(kbps, 'bandwidth probe')) {
        return;
    }

    const currentProfile = STREAM_LADDER[currentIndex];
    if (getQualityRank(currentProfile) >= QUALITY_ORDER.length - 1) {
        ABRState.consecutiveHighCount = 0;
        return;
    }

    const threshold = UPGRADE_THRESHOLDS_KBPS[getQualityKey(currentProfile)];
    if (!threshold) return;

    const nextStream = getNextQualityStreamName(currentProfile);
    if (!nextStream) return;

    if (kbps >= threshold) {
        ABRState.consecutiveHighCount++;
        ABRState.consecutiveLowCount = 0;
        ABRState.consecutiveProbeLowCount = 0;
        logAbrProbeProgress(`[ABR] Upgrade probe good x${ABRState.consecutiveHighCount}/${CONFIG.UPGRADE_CONFIRM_COUNT}: ${kbps} >= ${threshold} kbps`);
        if (ABRState.consecutiveHighCount >= CONFIG.UPGRADE_CONFIRM_COUNT) {
            performUpgrade(nextStream, `probe bandwidth stable: ${kbps} >= ${threshold} kbps`);
        }
    } else {
        logAbrProbeProgress(`[ABR] Upgrade probe not enough: ${kbps} < ${threshold} kbps`);
        ABRState.consecutiveHighCount = 0;
    }
}


function performDowngrade(reason, force = false, measuredKbps = 0) {
    if (!autoSelectActive) return;

    let targetStream = CONFIG.DEFAULT_FALLBACK_STREAM;

    if (force) {
        console.warn(`[ABR] urgent DOWNGRADE triggered! ${reason}. ${ABRState.currentStream} -> ${targetStream}`);
        ABRState.isSwitching = true;
        safeSwitchToStream(targetStream, 'downgrade');
        return;
    }

    if (ABRState.isSwitching) {
        console.warn(`[ABR] Ignored Downgrade (${reason}) because switching is in progress.`);
        return;
    }

    if (checkCooldown()) {
        console.log(`[ABR] Downgrade ignored due to cooldown. Reason: ${reason}`);
        return;
    }

    const currentIndex = getCurrentStreamIndex();
    if (currentIndex <= 0) {
        ABRState.consecutiveLowCount = 0;
        return;
    }

    if (shouldRollbackUpgrade()) {
        targetStream = ABRState.lastUpgradeFromStream;
        ABRState.upgradeBackoffUntil = Date.now() + CONFIG.UPGRADE_BACKOFF_MS;
        console.warn(`[ABR] rollback upgrade during probation. Backoff until ${new Date(ABRState.upgradeBackoffUntil).toISOString()}`);
    } else if (measuredKbps > 0) {
        targetStream = selectDowngradeStreamForCurrent(measuredKbps) || getPreviousQualityStreamName(currentIndex);
    } else {
        targetStream = getPreviousQualityStreamName(currentIndex);
    }

    const targetIndex = STREAM_LADDER.findIndex(s => s.name === targetStream);
    const currentRank = getQualityRank(STREAM_LADDER[currentIndex]);
    const targetRank = getQualityRank(STREAM_LADDER[targetIndex]);
    if (targetStream !== ABRState.currentStream && targetIndex >= 0 && targetRank >= 0 && targetRank < currentRank) {
        console.warn(`[ABR] DOWNGRADE triggered! Reason: ${reason}. ${ABRState.currentStream} -> ${targetStream}`);
        ABRState.isSwitching = true;
        safeSwitchToStream(targetStream, 'downgrade');
    }
}

function performUpgrade(targetStream, reason) {
    if (!autoSelectActive) return;
    if (ABRState.isSwitching) return;
    const audioOnly = isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
    if (!audioOnly && checkCooldown()) {
        console.log(`[ABR] Upgrade ignored due to cooldown. Reason: ${reason}`);
        return;
    }
    if (!playbackHealthyForUpgrade()) {
        console.log(`[ABR] Upgrade ignored because playback is not healthy. Reason: ${reason}`);
        return;
    }

    const currentIndex = getCurrentStreamIndex();
    const currentRank = getQualityRank(STREAM_LADDER[currentIndex]);
    if (currentRank >= QUALITY_ORDER.length - 1) return;

    const targetIndex = STREAM_LADDER.findIndex(s => s.name === targetStream);
    const targetRank = getQualityRank(STREAM_LADDER[targetIndex]);
    if (targetStream && targetIndex >= 0 && targetRank === currentRank + 1) {
        console.log(`[ABR] UPGRADE triggered! Reason: ${reason}. ${ABRState.currentStream} -> ${targetStream}`);
        ABRState.isSwitching = true;
        ABRState.lastUpgradeFromStream = ABRState.currentStream;
        ABRState.lastUpgradeTime = Date.now();
        safeSwitchToStream(targetStream, 'upgrade');
    }
}

/* ------------------------
   Player Lifecycle & Events
   ------------------------ */

const playerHandlers = {
  debug: null,
  webrtcstats: null,
  events: new Map()
};

function destroyPlayer() {
  if (monitorTimer) { clearInterval(monitorTimer); monitorTimer = null; }
  clearAudioOnlyReconnectTimer();
  localAudioOnlyMode = false;
  localAudioOnlySourceStream = null;
  if (typeof stopSnapshotStoreLoop === 'function') stopSnapshotStoreLoop();
  try {
    if (tcplayer) {
      if (playerHandlers.debug) tcplayer.off('debug', playerHandlers.debug);
      if (playerHandlers.webrtcstats) tcplayer.off('webrtcstats', playerHandlers.webrtcstats);
      for (const [evt, h] of playerHandlers.events) {
        tcplayer.off(evt, h);
      }
    }
  } catch(e){}
  try { if (tcplayer && tcplayer.dispose) tcplayer.dispose(); } catch(e){}
  tcplayer = null;
  cleanupPlayerDom();
}

function attachPlayer(options) {
  destroyPlayer();
  applyCanvasLayout(currentCanvasProfile);
  createVideoElementIfMissing();

  const audioOnly = !!options.audioOnly || isAudioOnlyStream(ABRState.currentStream, options.source);
  localAudioOnlyMode = audioOnly;
  localAudioOnlySourceStream = audioOnly ? (options.audioOnlySourceStream || getBareStreamNameFromUrl(options.source) || null) : null;
  setVideoPlaybackVisible(!audioOnly);
  
  const cfg = {
    autoplay: true,
    webrtcConfig: {
      connectTimeout: 5,
      connectRetryDelay: 1,
      connectRetryCount: 1,
      receiveVideo: !audioOnly,
      receiveAudio: true,
      fallback: false,
      showLog: false
    },
    language: 'zh-CN',
    reportable: false,
    sources: [options.source]
  };

  try { tcplayer = new TCPlayer('player-container-id', cfg); } 
  catch (e) { 
      console.error('TCPlayer init fail', e); 
      ABRState.isSwitching = false;
      return; 
  }
  applyPlayerAudioSettings();
  try {
      tcplayer.ready(function() {
          applyPlayerAudioSettings();
          const playResult = tcplayer.play && tcplayer.play();
          if (playResult && typeof playResult.catch === 'function') {
              playResult.catch(err => console.warn(`[PlayerAudio] play request rejected: ${err && err.message ? err.message : err}`));
          }
      });
  } catch (e) {}

  let stBuffTime = 0;
  playerHandlers.debug = async function(event) {
    try {
      const d = event && event.data;
      if (!d) return;
      if (d.code === 1009) {
          stBuffTime = Date.now();
      } 
      else if (d.code === 1010) {
          let buffTime = Date.now() - stBuffTime;
          if (buffTime > CONFIG.STALL_THRESHOLD_MS) {
              console.warn(`[Stall] Heavy lag detected: ${buffTime}ms`);
              ABRState.lastStallTime = Date.now();
              markNetworkIssue(`stall ${buffTime}ms`);
              performDowngrade(`Stall > 5000ms (${buffTime}ms)`, false);
              ApiPostPlayLag(currentWebrtcUrl, buffTime);
          }
      }
    } catch(e){}
  };
  tcplayer.on('debug', playerHandlers.debug);

  playerHandlers.webrtcstats = function(event){
    try {
      const data = event.data;
      const vBitrate = (data.video && data.video.bitrate) ? parseInt(data.video.bitrate / 1000) : 0;
      ABRState.currentReceiveKbps = vBitrate; 
      logAudioStats(data);
      if (hasActiveAudioStats(data && data.audio)) {
          ABRState.lastErrorCode = null;
          audioOnlyReconnectAttempts = 0;
          clearAudioOnlyReconnectTimer();
      }
      
      if (typeof onPlayStats === 'function') onPlayStats(data);
    } catch(e){}
  };
  tcplayer.on('webrtcstats', playerHandlers.webrtcstats);

  const commonEvents = ['loadstart','error','playing','play','pause','ended'];
  commonEvents.forEach(function(evt){
    const handler = async function(event){
        if (evt === 'play') {
          startPlayTime = Date.now();
        } else if (evt === 'playing') {
          const runtimeAudioOnly = audioOnly || localAudioOnlyMode || isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
          if (!runtimeAudioOnly) {
            updateCanvasProfile(tcplayer.videoWidth(), tcplayer.videoHeight());
          }
          const diffMs = Date.now() - startPlayTime;
          const playback = describePlaybackStream(tcplayer.videoWidth(), tcplayer.videoHeight());
          const sourceSuffix = playback.source && playback.source !== playback.stream ? `, source=${playback.source}` : '';
          console.log(`[Player] playing stream=${playback.stream}${sourceSuffix}, quality=${playback.quality}, level=${playback.level}, codec=${playback.codec}, resolution=${playback.resolution}, loadTime=${diffMs} ms`);
          applyPlayerAudioSettings();
          if (runtimeAudioOnly) {
            setVideoPlaybackVisible(false);
            console.log('[Player] audio-only stream active, video playback stopped.');
            if (typeof renderAudioFallbackSnapshot === 'function') {
              renderAudioFallbackSnapshot(ABRState.currentStream);
            }
          } else {
            setVideoPlaybackVisible(true);
            unfreezeLastFrame();
            if (shouldStoreFallbackSnapshot(ABRState.currentStream) && typeof startSnapshotStoreLoop === 'function') {
              startSnapshotStoreLoop(ABRState.currentStream);
            } else if (typeof stopSnapshotStoreLoop === 'function') {
              stopSnapshotStoreLoop();
            }
          }

          let diffFromLastPlay = -1;
          if (startPlayTimeLast !== -1){
              diffFromLastPlay = startPlayTime - startPlayTimeLast;
          }
          if (diffMs >= 50) {
              ApiPostStartPlay(currentWebrtcUrl, true, "ok", diffMs, diffFromLastPlay, false);
          }
          startPlayTimeLast = new Date();
          
          if (playTimer) clearInterval(playTimer);
          playTimer = setInterval(() => {
              ApiPostEndPlay(currentWebrtcUrl, 6000);
          }, 6000);

          ABRState.isSwitching = false;
          ABRState.consecutiveHighCount = 0;

        } else if (evt === 'error') {
            const errCode = event && event.data && event.data.code;
            const audioOnlyError = isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
            const logFn = audioOnlyError ? console.warn : console.error;
            logFn(`[Player Error] CODE:${errCode}`);
            ABRState.lastErrorTime = Date.now();
            ABRState.lastErrorCode = errCode;
            
            if (isFatalPlayerError(errCode)) {
                if (audioOnlyError) {
                    console.warn(`[ABR] Ignored player error ${errCode} on audio-only fallback stream.`);
                    clearPlayerErrorState();
                    ABRState.isSwitching = false;
                    if (!localAudioOnlyMode) {
                        scheduleAudioOnlyReconnect(`Player Error Code: ${errCode}`);
                    }
                    return;
                }
                cleanupPlayerDom();
                if (errCode === -2004 && fallbackUnderscoreToLegacy(`Player Error Code: ${errCode}`)) {
                    return;
                }
                // console.error(`[Health] Marking ${ABRState.currentStream} as DEAD (Offline).`);
                // ABRState.isSwitching = false;

                // const currentProfile = getStreamProfile(ABRState.currentStream);
                // if (currentProfile) {
                //     currentProfile.online = false;
                // }
                performDowngrade(`Player Error Code: ${errCode}`, true);
            }
      }
    };
    playerHandlers.events.set(evt, handler);
    tcplayer.on(evt, handler);
  });

  if (!monitorTimer) { 
    startMonitorLoop();
  }
}

function startMonitorLoop() {
    if (monitorTimer) clearInterval(monitorTimer);

    resetAbrCounters();
    
    monitorTimer = setInterval(async () => {
        if (!tcplayer || !autoSelectActive || !ABRState.currentStream) return;

        if (ABRState.isSwitching) return;

        const audioOnly = isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
        if (!audioOnly && checkCooldown()) {
            resetAbrCounters();
            return;
        }

        const currentIndex = getCurrentStreamIndex();
        if (currentIndex < 0) return;

        await maybeProbeAndUpgrade();
    }, CONFIG.MONITOR_INTERVAL_MS);
}

async function safeSwitchToStream(streamKey, switchType = 'manual') {
  const targetAudioOnlyByName = isAudioOnlyStream(streamKey, '');
  const playbackStreamKey = targetAudioOnlyByName
      ? getAudioOnlyPlaybackStreamName(streamKey)
      : getPlaybackStreamForRequest(streamKey);

  const url = await getWebrtcUrl(playbackStreamKey);
  if (!url) {
      console.warn(`[Switch] Failed to get URL for ${playbackStreamKey}`);
      ABRState.isSwitching = false;
      return;
  }
  if (typeof getBareStreamNameFromUrl === 'function') {
      const urlStream = getBareStreamNameFromUrl(url);
      if (urlStream && urlStream !== playbackStreamKey) {
          console.warn(`[Switch] Refuse mismatched URL. requested=${playbackStreamKey}, logical=${streamKey}, urlStream=${urlStream}`);
          ABRState.isSwitching = false;
          return;
      }
  }

  const targetAudioOnly = targetAudioOnlyByName || isAudioOnlyStream(streamKey, url);
  if (!targetAudioOnly) {
      exitLocalAudioOnlyMode();
      audioOnlyReconnectAttempts = 0;
      clearAudioOnlyReconnectTimer();
  }
  if (targetAudioOnly) {
      freezeLastFrame();
      setVideoPlaybackVisible(false);
  } else {
      freezeLastFrame();
      setVideoPlaybackVisible(true);
  }

  const sourceSuffix = playbackStreamKey !== streamKey ? ` via ${playbackStreamKey}` : '';
  console.log(`[Switch] Executing switch to: ${streamKey}${sourceSuffix}`);
  ABRState.currentStream = streamKey;
  currentWebrtcUrl = url;
  const webrtcInput = document.getElementById('webrtc');
  if (webrtcInput) webrtcInput.value = url;
  if (switchType !== 'audio_retry') {
      ABRState.lastSwitchTime = Date.now();
  }
  if (switchType !== 'upgrade') {
      ABRState.lastUpgradeFromStream = null;
      ABRState.lastUpgradeTime = 0;
  }
  clearUpgradeProbeState();
  setCookie('phil_stream_choice', streamKey, 1);
  setCookie('phil_stream_url', url, 1);

  attachPlayer({
      source: url,
      audioOnly: targetAudioOnly,
      audioOnlySourceStream: targetAudioOnly ? playbackStreamKey : ''
  });
  if (!targetAudioOnly) {
      unfreezeLastFrame();
  }
}
