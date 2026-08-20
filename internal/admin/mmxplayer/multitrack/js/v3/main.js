/**
 * main.js
 * UI bindings for site-based ABR playback.
 */

window.VConsole && new window.VConsole();

let pageSiteName = currentSiteName;
let pageViewName = 'fwh';
let pageStreamNames = null;
let pageEmbeddedMode = false;
let pagePreviewMode = '';
let previewGridPlayers = [];
const PREVIEW_GRID_CONNECTING_NOTICE_MS = 12000;
const PREVIEW_GRID_HARD_TIMEOUT_MS = 45000;

const SITE_OPTIONS = [
  { value: 'studio_3drush' },
  { value: 'studio_gsp2w' }
];

const VIEW_OPTIONS = [
  { value: 'fwh' },
  { value: 'fwv' }
];

const DEFAULT_VIEW_BY_SITE = {
  studio_3drush: 'fwh',
  studio_gsp2w: 'fwv'
};

const SITE_BY_STREAM = {
  '3drush-fwh': 'studio_3drush',
  '3drush-fwv': 'studio_3drush',
  'gsp2w-fwv': 'studio_gsp2w',
  'gsp2w-fwh': 'studio_gsp2w',
};

const QUALITY_LEVELS = [
  { value: 'auto', label: 'auto' },
  { value: 'high', label: '1080P' },
  { value: 'standard', label: '720P' },
  { value: 'economic', label: '360P' },
  { value: 'bottom', label: 'audio' }
];

function normalizeSiteName(siteName) {
  const value = (siteName || '').trim();
  if (SITE_OPTIONS.some(site => site.value === value)) return value;
  if (value === 'studio_gwp2w' || value === 'gwp2w') return 'studio_gsp2w';
  if (SITE_BY_STREAM[value]) return SITE_BY_STREAM[value];
  // Handle stream variant names like "gsp2w-fwv_standard"
  for (const [stream, site] of Object.entries(SITE_BY_STREAM)) {
    if (value.startsWith(stream + '_')) return site;
  }
  return SITE_OPTIONS[0].value;
}

function normalizeViewName(viewName) {
  const value = (viewName || '').trim().toLowerCase();
  if (VIEW_OPTIONS.some(view => view.value === value)) return value;
  return VIEW_OPTIONS[0].value;
}

function defaultViewForSite(siteName) {
  return DEFAULT_VIEW_BY_SITE[normalizeSiteName(siteName)] || VIEW_OPTIONS[0].value;
}

function viewNameFromStream(streamName) {
  const value = (streamName || '').trim().toLowerCase();
  if (value.includes('fwv')) return 'fwv';
  if (value.includes('fwh')) return 'fwh';
  return '';
}

function streamBaseNameForSiteView(siteName, viewName) {
  const siteBase = normalizeSiteName(siteName).replace(/^studio_/, '');
  return `${siteBase}-${normalizeViewName(viewName)}`;
}

function legacyStreamName(streamName) {
  const value = (streamName || '').trim();
  const match = value.match(/^(3drush|gsp2w)_(fwh|fwv)(.*)$/);
  if (!match) return value;
  return `${match[1]}-${match[2]}${match[3]}`;
}

function streamNamesFromSelection(siteName, viewName) {
  const site = streamBaseNameForSiteView(siteName, viewName);
  return {
    high: site,
    standard: `${site}_standard`,
    economic: `${site}_economic`,
    bottom: `${site}_audio`
  };
}

function streamNamesFromParams(params, fallback) {
  const streamNames = Object.assign({}, fallback || {});
  const mappings = [
    ['bottom', 'bottom'],
    ['economic', 'economic'],
    ['standard', 'standard'],
    ['high', 'high']
  ];
  let hasOverride = false;
  for (const [paramName, key] of mappings) {
    const value = (params.get(paramName) || '').trim();
    if (!value) continue;
    streamNames[key] = value;
    hasOverride = true;
  }
  return hasOverride ? streamNames : fallback;
}

function streamNamesForPlayback(streamNames) {
  if (!window.__useLegacyStreamNames) return streamNames;
  return Object.keys(streamNames || {}).reduce((out, key) => {
    out[key] = legacyStreamName(streamNames[key]);
    return out;
  }, {});
}

function normalizeQualityChoice(choice) {
  const value = (choice || '').trim().toLowerCase();
  if (value === '1080p' || value === '1080') return 'high';
  if (value === '720p' || value === '720') return 'standard';
  if (value === '360p' || value === '360') return 'economic';
  if (value === 'audio') return 'bottom';
  return QUALITY_LEVELS.some(item => item.value === value) ? value : 'auto';
}

function isTruthyParam(value) {
  const normalized = String(value || '').trim().toLowerCase();
  return normalized === '1' || normalized === 'true' || normalized === 'yes' || normalized === 'on';
}

function applyEmbeddedMode(params) {
  pageEmbeddedMode = isTruthyParam(params.get('embedded'));
  pagePreviewMode = (params.get('preview') || '').trim().toLowerCase();
  if (!pageEmbeddedMode) return;
  document.documentElement.classList.add('embedded-player');
  document.body.classList.add('embedded-player');
  if (pagePreviewMode === 'grid') {
    document.documentElement.classList.add('preview-grid-mode');
    document.body.classList.add('preview-grid-mode');
  }
  if (isTruthyParam(params.get('muted')) && typeof setPlayerMuted === 'function') {
    setPlayerMuted(true);
  }
}

function buildSelectableStreams() {
  return SITE_OPTIONS.map(site => ({ name: site.value, label: site.value }));
}

function buildSelectableViews() {
  return VIEW_OPTIONS.map(view => ({ name: view.value, label: view.value }));
}

function populateStreamSelect(siteName, viewName) {
  pageSiteName = normalizeSiteName(siteName);
  pageViewName = normalizeViewName(viewName);
  pageStreamNames = streamNamesFromSelection(pageSiteName, pageViewName);
  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));

  const select = document.getElementById('stream-select');
  if (!select) return;

  select.innerHTML = '';
  for (const site of buildSelectableStreams()) {
    select.add(new Option(site.label, site.name, false, site.name === pageSiteName));
  }
  select.value = pageSiteName;

  if (typeof M !== 'undefined') {
    const inst = M.FormSelect.getInstance(select);
    if (inst) inst.destroy();
    M.FormSelect.init(select);
  }

  populateViewSelect();
  populateQualitySelect();
}

function populateViewSelect() {
  const select = document.getElementById('view-select');
  if (!select) return;

  select.innerHTML = '';
  for (const view of buildSelectableViews()) {
    select.add(new Option(view.label, view.name, false, view.name === pageViewName));
  }
  select.value = pageViewName;

  if (typeof M !== 'undefined') {
    const inst = M.FormSelect.getInstance(select);
    if (inst) inst.destroy();
    M.FormSelect.init(select);
  }
}

function populateQualitySelect() {
  const select = document.getElementById('quality-select');
  if (!select) return;

  const current = normalizeQualityChoice(getCookie('phil_quality_choice') || select.value || 'auto');
  select.innerHTML = '';
  for (const item of QUALITY_LEVELS) {
    select.add(new Option(item.label, item.value, false, item.value === current));
  }
  select.value = current;

  if (typeof M !== 'undefined') {
    const inst = M.FormSelect.getInstance(select);
    if (inst) inst.destroy();
    M.FormSelect.init(select);
  }
}

function selectedQualityLevel() {
  return normalizeQualityChoice($('#quality-select').val() || getCookie('phil_quality_choice'));
}

function streamForQualityLevel(quality) {
  quality = normalizeQualityChoice(quality);
  if (quality === 'auto') return null;
  if (quality === 'bottom') return getPreferredStreamForQuality('bottom') || CONFIG.DEFAULT_FALLBACK_STREAM;
  const base = streamNamesFromSelection(pageSiteName, pageViewName).high;
  const layer = { high: 'q0', standard: 'q1', economic: 'q2' }[quality];
  return layer ? `${base}_${layer}` : base;
}

function previewGridStreamForQuality(quality) {
  quality = normalizeQualityChoice(quality);
  if (quality === 'bottom' && typeof getPlaybackStreamForRequest === 'function') {
    return getPlaybackStreamForRequest(streamForQualityLevel('bottom'));
  }
  if (quality === 'standard') return getPreferredStreamForQuality('standard');
  return streamForQualityLevel(quality);
}

function streamNameForPlayback(streamName) {
  const value = (streamName || '').trim();
  if (!value) return '';
  return window.__useLegacyStreamNames ? legacyStreamName(value) : value;
}

function syncStreamSelectValue(streamName) {
  const select = document.getElementById('stream-select');
  if (!select) return;
  const siteName = normalizeSiteName(streamName || pageSiteName);
  if ([...select.options].some(option => option.value === siteName)) {
    select.value = siteName;
    if (typeof M !== 'undefined') {
      const inst = M.FormSelect.getInstance(select);
      if (inst) inst.destroy();
      M.FormSelect.init(select);
    }
  }
}

function syncViewSelectValue(viewName) {
  const select = document.getElementById('view-select');
  if (!select) return;
  const selectedView = normalizeViewName(viewName || pageViewName);
  if ([...select.options].some(option => option.value === selectedView)) {
    select.value = selectedView;
    if (typeof M !== 'undefined') {
      const inst = M.FormSelect.getInstance(select);
      if (inst) inst.destroy();
      M.FormSelect.init(select);
    }
  }
}

function clearSavedStreamChoice() {
  setCookie('phil_stream_choice', '', -1);
  setCookie('phil_stream_url', '', -1);
}

async function startSelectedQuality() {
  const quality = selectedQualityLevel();
  setCookie('phil_quality_choice', quality, 1);
  if (quality === 'auto') {
    await startSiteAbr(pageSiteName);
    return;
  }

  await startManualStream(streamForQualityLevel(quality));
}

function selectSite(siteName) {
  const previousSiteName = pageSiteName;
  pageSiteName = normalizeSiteName(siteName);
  pageViewName = defaultViewForSite(pageSiteName);
  pageStreamNames = streamNamesFromSelection(pageSiteName, pageViewName);
  setCookie('phil_site_choice', pageSiteName, 1);
  setCookie('phil_view_choice', pageViewName, 1);
  if (pageSiteName !== previousSiteName) clearSavedStreamChoice();
  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
  syncStreamSelectValue(pageSiteName);
  syncViewSelectValue(pageViewName);
  // Auto-play on site change
  destroyPlayer();
  currentWebrtcUrl = null;
  var webrtcInput = document.getElementById('webrtc');
  if (webrtcInput) webrtcInput.value = '';
  startSelectedQuality();
}

function selectView(viewName) {
  const previousViewName = pageViewName;
  pageViewName = normalizeViewName(viewName);
  pageStreamNames = streamNamesFromSelection(pageSiteName, pageViewName);
  setCookie('phil_view_choice', pageViewName, 1);
  if (pageViewName !== previousViewName) clearSavedStreamChoice();
  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
  syncViewSelectValue(pageViewName);
  // Auto-play on view change
  destroyPlayer();
  currentWebrtcUrl = null;
  var webrtcInput = document.getElementById('webrtc');
  if (webrtcInput) webrtcInput.value = '';
  startSelectedQuality();
}

function applyLegacyStreamNameMode() {
  window.__useLegacyStreamNames = true;
  pageStreamNames = streamNamesFromSelection(pageSiteName, pageViewName);
  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
}

async function startSiteAbr(siteName) {
  console.log('[Init] Checking capabilities...');
  await initStatApiConfig();
  await detectHevcCapability();

  if (siteName && normalizeSiteName(siteName) !== pageSiteName) {
    selectSite(siteName);
  }
  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
  autoSelectActive = true;
  setCookie('phil_quality_choice', 'auto', 1);
  ABRState.lastSwitchTime = Date.now();

   const cachedStream = getCookie('phil_stream_choice');
   const cachedProfile = getStreamProfile(cachedStream);
   const standard = getPreferredStandardProfile();
   let pick = cachedProfile ? cachedProfile.name : (standard ? standard.name : currentSiteName);
  ABRState.currentStream = pick;
  syncStreamSelectValue(pick);
  syncViewSelectValue(viewNameFromStream(pick));
  console.log(`[Init] selected site=${pageSiteName}, view=${pageViewName}, quality=auto, stream=${pick}`);
  const playbackStream = typeof getPlaybackStreamForRequest === 'function'
      ? getPlaybackStreamForRequest(pick)
      : pick;
  const url = await getWebrtcUrl(playbackStream);

  if (url) {
      currentWebrtcUrl = url;
      document.getElementById('webrtc').value = url;
      attachPlayer({
        source: url,
        audioOnly: isAudioOnlyStream(pick, ''),
        audioOnlySourceStream: playbackStream
      });
  } else {
      console.error('Auto start failed: No URL');
  }

  startMonitorLoop();
}

async function startManualStream(streamName) {
  streamName = streamNameForPlayback(streamName);
  if (!streamName) return;

  console.log('[Init] Checking capabilities...');
  await initStatApiConfig();
  await detectHevcCapability();

  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
  autoSelectActive = false;
  ABRState.currentStream = streamName;
  syncStreamSelectValue(streamName);
  syncViewSelectValue(viewNameFromStream(streamName));
  const profile = getStreamProfile(streamName);
  if (profile) setCookie('phil_quality_choice', getQualityKey(profile), 1);
  console.log(`[Init] selected site=${pageSiteName}, view=${pageViewName}, quality=${profile ? getQualityKey(profile) : 'unknown'}, stream=${streamName}`);

  const playbackStream = typeof getPlaybackStreamForRequest === 'function'
      ? getPlaybackStreamForRequest(streamName)
      : streamName;
  const url = await getWebrtcUrl(playbackStream);
  if (url) {
      currentWebrtcUrl = url;
      document.getElementById('webrtc').value = url;
      attachPlayer({
        source: url,
        audioOnly: isAudioOnlyStream(streamName, ''),
        audioOnlySourceStream: playbackStream
      });
  } else {
      console.error('Manual start failed: No URL');
  }
}

function clearPreviewGridPlayers() {
  for (const player of previewGridPlayers) {
    try {
      if (player && typeof player.dispose === 'function') player.dispose();
      else if (player && typeof player.destroy === 'function') player.destroy();
    } catch (e) {}
  }
  previewGridPlayers = [];
}

function setPreviewTileState(tile, state, message) {
  if (!tile) return;
  tile.classList.remove('is-loading', 'is-playing', 'is-error');
  tile.classList.add(`is-${state}`);
  const status = tile.querySelector('.preview-grid-status');
  if (status) status.textContent = message || state;
}

function isRecoverablePreviewPlayError(err) {
  const name = String(err && err.name || '').toLowerCase();
  const message = String(err && err.message || '').toLowerCase();
  return name === 'aborterror' || message.includes('interrupted by a call to pause');
}

function togglePreviewGridTileZoom(grid, tile) {
  if (!grid || !tile) return;
  const alreadyZoomed = grid.classList.contains('is-zoomed') && tile.classList.contains('is-zoomed');
  grid.classList.toggle('is-zoomed', !alreadyZoomed);
  grid.querySelectorAll('.preview-grid-tile.is-zoomed').forEach(function(item) {
    item.classList.remove('is-zoomed');
  });
  if (!alreadyZoomed) {
    tile.classList.add('is-zoomed');
  }
}

async function startPreviewGrid() {
  clearPreviewGridPlayers();
  destroyPlayer();
  await detectHevcCapability();
  setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
  if (typeof setPlayerMuted === 'function') setPlayerMuted(true);

  const host = document.querySelector('.player-card') || document.body;
  host.innerHTML = '<div id="preview-grid" class="preview-grid"></div>';
  const grid = document.getElementById('preview-grid');
  if (!grid) return;

  const specs = [
    { label: '1080P', quality: 'high' },
    { label: '720P', quality: 'standard' },
    { label: '360P', quality: 'economic' },
    { label: 'audio', quality: 'bottom' }
  ];

  await Promise.all(specs.map(async function(spec, index) {
    const stream = previewGridStreamForQuality(spec.quality);
    const audioOnly = spec.quality === 'bottom';
    const tile = document.createElement('div');
    const playerId = `preview-grid-player-${index}`;
    tile.className = 'preview-grid-tile is-loading';
    tile.innerHTML = `
      <div class="preview-grid-title">
        <span>${spec.label}</span>
        <small>${stream || 'no stream'}</small>
      </div>
      <video id="${playerId}" class="preview-grid-video" playsinline webkit-playsinline muted></video>
      <div class="preview-grid-audio-label">${audioOnly ? 'audio only via 360P' : ''}</div>
      <div class="preview-grid-error-mark">X</div>
      <div class="preview-grid-status">loading</div>
    `;
    grid.appendChild(tile);
    tile.addEventListener('dblclick', function(event) {
      event.preventDefault();
      togglePreviewGridTileZoom(grid, tile);
    });

    if (!stream) {
      setPreviewTileState(tile, 'error', 'no stream');
      return;
    }

    const url = await getWebrtcUrl(stream);
    if (!url) {
      setPreviewTileState(tile, 'error', 'no url');
      return;
    }

    try {
      let connectingTimer = null;
      let failTimer = null;
      const player = new TCPlayer(playerId, {
        autoplay: true,
        muted: true,
        webrtcConfig: {
          connectTimeout: 12,
          connectRetryDelay: 2,
          connectRetryCount: 2,
          receiveVideo: !audioOnly,
          receiveAudio: true,
          fallback: false,
          showLog: false
        },
        language: 'zh-CN',
        reportable: false,
        sources: [url]
      });
      previewGridPlayers.push(player);
      connectingTimer = setTimeout(function() {
        if (!tile.classList.contains('is-playing') && !tile.classList.contains('is-error')) {
          setPreviewTileState(tile, 'loading', 'connecting');
        }
      }, PREVIEW_GRID_CONNECTING_NOTICE_MS);
      failTimer = setTimeout(function() {
        if (!tile.classList.contains('is-playing')) {
          setPreviewTileState(tile, 'error', 'timeout');
        }
      }, PREVIEW_GRID_HARD_TIMEOUT_MS);
      player.on('playing', function() {
        if (connectingTimer) clearTimeout(connectingTimer);
        if (failTimer) clearTimeout(failTimer);
        setPreviewTileState(tile, 'playing', 'playing');
      });
      player.on('error', function(event) {
        if (connectingTimer) clearTimeout(connectingTimer);
        if (failTimer) clearTimeout(failTimer);
        const code = event && event.data && event.data.code;
        setPreviewTileState(tile, 'error', code ? `error ${code}` : 'error');
      });
      player.ready(function() {
        try {
          if (typeof player.muted === 'function') player.muted(true);
          if (typeof player.volume === 'function') player.volume(0);
          const result = player.play && player.play();
          if (result && typeof result.catch === 'function') {
            result.catch(function(err) {
              if (isRecoverablePreviewPlayError(err)) {
                if (!tile.classList.contains('is-playing') && !tile.classList.contains('is-error')) {
                  setPreviewTileState(tile, 'loading', 'starting');
                }
                return;
              }
              setPreviewTileState(tile, 'error', err && err.message ? err.message : 'play rejected');
            });
          }
        } catch (err) {
          setPreviewTileState(tile, 'error', err && err.message ? err.message : 'play failed');
        }
      });
    } catch (err) {
      setPreviewTileState(tile, 'error', err && err.message ? err.message : 'init failed');
    }
  }));
}

$('#quality-select').on('change', async function() {
  const quality = selectedQualityLevel();
  setCookie('phil_quality_choice', quality, 1);
  clearSavedStreamChoice();
  destroyPlayer();
  currentWebrtcUrl = null;
  const webrtcInput = document.getElementById('webrtc');
  if (webrtcInput) webrtcInput.value = '';
  await startSelectedQuality();
});

$('#stream-select').on('change', debounce(async function() {
  selectSite($('#stream-select').val());
}, 400));

$('#view-select').on('change', debounce(async function() {
  selectView($('#view-select').val());
}, 400));

$('#startPlay').on('click', async function() {
  destroyPlayer();
  currentWebrtcUrl = null;
  const webrtcInput = document.getElementById('webrtc');
  if (webrtcInput) webrtcInput.value = '';
  await startSelectedQuality();
});

$('#stopPlay').on('click', function() {
  destroyPlayer();
  autoSelectActive = false;
  if (monitorTimer) { clearInterval(monitorTimer); monitorTimer = null; }
  startPlayTime = 0;

  $('.stat-info').hide();
  $('ul.stat').empty();
});

let lastPlayStats = null;
let lastVideoStats = null;
let lastAudioStats = null;

function onPlayStats(data) {
  lastPlayStats = data || null;
  const stream = typeof ABRState !== 'undefined' ? ABRState.currentStream : '';
  const video = data && data.video;
  const audio = extractAudioStats(data);
  const now = Date.now();

  if (hasStatData(video)) {
    lastVideoStats = { stream, data: video, updatedAt: now };
  }
  if (hasStatData(audio)) {
    lastAudioStats = { stream, data: audio, updatedAt: now };
  }
  renderPlayStats(lastPlayStats);
}

function renderPlayStats(data) {
  const audioOnly = isCurrentAudioOnly();
  const statSections = [
    { title: 'Video', data: buildVideoStatData(data, audioOnly) },
    { title: 'Audio', data: buildAudioStatData(data, audioOnly) }
  ];
  let ulHtml = '';

  statSections.forEach(function(section) {
    ulHtml += `<li class="stat-section-title"><div class="title">${section.title}</div></li>`;
    if (!section.data || Object.keys(section.data).length === 0) {
      ulHtml += `<li><div class="label">status</div><div class="text">No data</div></li>`;
      return;
    }

    Object.keys(section.data).forEach(function(key) {
      const val = formatStatValue(key, section.data[key]);
      ulHtml += `<li><div class="label">${escapeStatText(key)}</div><div class="text">${escapeStatText(val)}</div></li>`;
    });
  });
  $('ul.stat').html(ulHtml);
}

function isCurrentAudioOnly() {
  try {
    return typeof isAudioOnlyStream === 'function' && typeof ABRState !== 'undefined' &&
      isAudioOnlyStream(ABRState.currentStream, currentWebrtcUrl);
  } catch(e) {
    return false;
  }
}

function buildVideoStatData(data, audioOnly) {
  if (audioOnly && typeof ABRState !== 'undefined') {
    return {
      status: 'audio-only stream',
      stream: ABRState.currentStream || '-',
      receiveVideo: false
    };
  }
  if (data && data.video && Object.keys(data.video).length > 0) return data.video;
  if (lastVideoStats && lastVideoStats.data) return lastVideoStats.data;
  return data && data.video;
}

function buildAudioStatData(data, audioOnly) {
  const audio = extractAudioStats(data);
  if (hasStatData(audio)) return audio;
  if (lastAudioStats && lastAudioStats.data &&
      (typeof ABRState === 'undefined' || lastAudioStats.stream === ABRState.currentStream)) {
    return Object.assign({}, lastAudioStats.data, {
      statsSource: 'cached current stream audio stats',
      statsAgeSeconds: Math.round((Date.now() - lastAudioStats.updatedAt) / 1000)
    });
  }
  if (!audioOnly || typeof ABRState === 'undefined') return data && data.audio;
  return {
    status: 'No active audio stats from Tencent player',
    stream: ABRState.currentStream || '-',
    quality: 'bottom',
    codec: 'OPUS',
    receiveAudio: true,
    receiveVideo: false,
    lastPlayerError: ABRState.lastErrorCode || '-',
    lastErrorAt: ABRState.lastErrorTime ? new Date(ABRState.lastErrorTime).toISOString() : '-'
  };
}

function extractAudioStats(data) {
  if (!data) return null;
  const candidates = [
    data.audio,
    data.audioStats,
    data.webrtc && data.webrtc.audio,
    data.stats && data.stats.audio
  ];
  for (let i = 0; i < candidates.length; i++) {
    if (hasStatData(candidates[i])) return candidates[i];
  }
  if (!data.video && looksLikeAudioStats(data)) return data;
  return null;
}

function hasStatData(value) {
  return !!(value && typeof value === 'object' && Object.keys(value).length > 0);
}

function looksLikeAudioStats(value) {
  if (!value || typeof value !== 'object') return false;
  return ['audioLevel', 'sampleRate', 'audioBitrate'].some(function(key) {
    return value[key] !== undefined && value[key] !== null && value[key] !== '';
  });
}

function formatStatValue(key, value) {
  if (value === null || value === undefined || value === '') return '-';
  if (key === 'bitrate' && Number.isFinite(Number(value))) {
    return `${(Number(value) / 1000).toFixed(0)} kbps`;
  }
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

function escapeStatText(value) {
  return String(value)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

$('#showStat').on('click', function() {
  if (!$('ul.stat').children().length) {
    renderPlayStats(lastPlayStats);
  }
  const panel = $('.stat-info');
  panel.css({
    'z-index': 120,
    'display': panel.is(':visible') ? 'none' : 'block'
  });
});
$('#mutePlay').on('click', function() {
  if (typeof togglePlayerMuted === 'function') togglePlayerMuted();
});
$('.close-icon').on('click', function() { $('.stat-info').hide(); });
$('#enterFullScreen').on('click', function() { try { if (tcplayer) tcplayer.requestFullscreen(true); } catch(e){} });

document.addEventListener('DOMContentLoaded', async function() {
    const params = new URLSearchParams(window.location.search);
    applyEmbeddedMode(params);
    const querySite = params.get('site') || '';
    const site = querySite || getCookie('phil_site_choice') || currentSiteName;
    const view = params.get('view') || viewNameFromStream(params.get('high') || querySite) || defaultViewForSite(site);
    const quality = params.get('quality');
    if (quality) setCookie('phil_quality_choice', normalizeQualityChoice(quality), 1);
    populateStreamSelect(site, view);
    const queryStreamNames = streamNamesFromParams(params, pageStreamNames);
    if (queryStreamNames !== pageStreamNames) {
      pageStreamNames = queryStreamNames;
      setStreamLadder(pageSiteName, streamNamesForPlayback(pageStreamNames));
      console.log(`[Init] applied stream ladder from URL: high=${pageStreamNames.high || ''}, standard=${pageStreamNames.standard || ''}, economic=${pageStreamNames.economic || ''}, bottom=${pageStreamNames.bottom || ''}`);
    }
    if (typeof updateMuteButton === 'function') updateMuteButton();
    if (pageEmbeddedMode && pagePreviewMode === 'grid') {
      await startPreviewGrid();
      return;
    }
    // Do not establish a media connection until the user presses Start.
    // Embedded autoplay is intentionally disabled in the admin dashboard.
});
window.addEventListener('beforeunload', function(){ destroyPlayer(); });
