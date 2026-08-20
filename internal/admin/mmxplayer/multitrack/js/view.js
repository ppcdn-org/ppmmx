var offerSdp = '';
var answerSdp = '';

var timePoint = {
  start: -1,
  request: -1,
  signal: -1,
  play: -1
};

function initView() {
  if (/Android|iPhone|iPad|iOS/i.test(navigator.userAgent)) {
    new VConsole();
  }

  var tabs = M.Tabs.init($('.tabs')[0]);
  tabs.select('info');
  document.body.scrollTop = document.documentElement.scrollTop = 0;

  M.FormSelect.init($('#type-select')[0]);

  $('#offer-copy').on('click', function () {
    copyToClipboard(offerSdp, {
      format: 'text/plain',
      onCopy: function () {
        M.toast({html: 'Copy success'})
      }
    });
  });

  $('#answer-copy').on('click', function () {
    copyToClipboard(answerSdp, {
      format: 'text/plain',
      onCopy: function () {
        M.toast({html: 'Copy success'})
      }
    });
  });

  if (livePlayer) {
    livePlayer.setPlayListener({
      onPlayEvent: onPlayEvent
    });
  }
}

function onPlayEvent(event, data) {
  var EVENT_CODE = {
    PLAY_EVT_STREAM_BEGIN: 1001,
    PLAY_EVT_SERVER_CONNECTED: 1002,
    PLAY_EVT_PLAY_BEGIN: 1003,
    PLAY_EVT_PLAY_STOP: 1004,
    PLAY_EVT_SERVER_RECONNECT: 1005,
    PLAY_EVT_STREAM_EMPTY: 1006,
    PLAY_EVT_REQUEST_PULL_BEGIN: 1007,
    PLAY_EVT_REQUEST_PULL_SUCCESS: 1008,
    PLAY_ERR_WEBRTC_FAIL: -2001,
    PLAY_ERR_REQUEST_PULL_FAIL: -2002,
    PLAY_ERR_PLAY_FAIL: -2003,
    PLAY_ERR_SERVER_DISCONNECT: -2004,
    PLAY_ERR_DECODE_FAIL: -2005
  };
  if (event === EVENT_CODE.PLAY_EVT_REQUEST_PULL_BEGIN) {
    onSdpData(data.localSdp);
    timePoint.start = performance.now();
  }
  if (event === EVENT_CODE.PLAY_EVT_REQUEST_PULL_SUCCESS) {
    timePoint.request = performance.now();
    onTimePointData(timePoint.start, timePoint.request, 'request');
    onSdpData(data.remoteSdp);
    onInfoData(data.remoteSdp);
  }
  if (event === EVENT_CODE.PLAY_EVT_SERVER_CONNECTED) {
    timePoint.signal = performance.now();
    onTimePointData(timePoint.start, timePoint.signal, 'signal');
  }
  if (event === EVENT_CODE.PLAY_EVT_PLAY_BEGIN) {
    timePoint.play = performance.now();
    onTimePointData(timePoint.start, timePoint.play, 'first-frame');
  }
  if (event === EVENT_CODE.PLAY_EVT_STREAM_BEGIN) {
    $('ul.stat').html('No data available');
  }
  if (event === EVENT_CODE.PLAY_ERR_SERVER_DISCONNECT || event === EVENT_CODE.PLAY_EVT_PLAY_STOP) {
    $('.stat-info').hide();
    timePoint = {
      start: -1,
      signal: -1,
      request: -1,
      play: -1
    };
  }
}

function onSdpData(data) {
  var html = '<table class="borderless"><tbody>';
  var sdpList = data.sdp.split('\r\n');
  for (var i = 0; i < sdpList.length; i++) {
    var lineNum = i + 1;
    var sdpText = sdpList[i];
    if (sdpText === '') {
      continue;
    }
    html += '<tr><td class="num">' + lineNum.toString().padStart(3, '0') + '</td><td>' + sdpText + '</td></tr>';
  }
  html += '</tbody></table>';
  if (data.type === 'offer') {
    offerSdp = data.sdp;
    $('#offer-table').html(html);
    $('#offer-copy').show();
  } else {
    answerSdp = data.sdp;
    $('#answer-table').html(html);
    $('#answer-copy').show();
  }
}

function onTimePointData(startTime, endTime, id) {
  if (startTime !== -1 && endTime !== -1) {
    $('#' + id).text((endTime - startTime).toFixed(2) + ' ms');
  }
}

function onInfoData(data) {
  var mcdId = '';
  var streamId = '';
  var iceUfrag = '';
  var icePwd = '';
  var serverInfo = '';

  var sdpList = data.sdp.split('\r\n');
  for (var i = 0; i < sdpList.length; i++) {
    var sdpText = sdpList[i];

    var reg = /(?:a=msid-semantic: WMS )(.+)/;
    var result = reg.exec(sdpText);
    if (result) {
      var username = result[1];
      var spos = username.indexOf('_');
      if (spos !== -1) {
        mcdId = username.substr(0, spos); 
        streamId = username.substr(spos + 1);
      }
      continue;
    }

    var reg = /(?:a=ice-ufrag:)(.+)/;
    var result = reg.exec(sdpText);
    if (result) {
      iceUfrag = result[1];
      continue;
    }

    var reg = /(?:a=ice-pwd:)(.+)/;
    var result = reg.exec(sdpText);
    if (result) {
      icePwd = result[1];
      continue;
    }

    var reg = /(?:a=candidate:foundation 1 udp)(.+)(?:typ)/;
    var result = reg.exec(sdpText);
    if (result) {
      serverInfo = result[1].trim();
      continue;
    }
  }
  
  var html = '<table><tbody>';
  html += '<tr><td colspan="2" class="head">Stream</td></tr>';
  html += '<tr><td>Mcd ID</td><td>' + mcdId + '</td></tr>';
  html += '<tr><td>Stream ID</td><td>' + streamId + '</td></tr>';
  html += '<tr><td>Ice Ufrag</td><td>' + iceUfrag + '</td></tr>';
  html += '<tr><td>Ice Pwd</td><td>' + icePwd + '</td></tr>';
  html += '<tr><td>Server Info</td><td>' + serverInfo + '</td></tr>';

  var bowserInfo = bowser.parse(navigator.userAgent);
  var browser = bowserInfo.browser;
  var os = bowserInfo.os;
  var platform = bowserInfo.platform;
  html += '<tr><td colspan="2" class="head">Environment</td></tr>';
  html += '<tr><td>Browser</td><td>' + browser.name + ' ' + browser.version + '</td></tr>';
  html += '<tr><td>OS</td><td>' + os.name + ' ' + os.version + '</td></tr>';
  html += '<tr><td>Platform</td><td>' + platform.type + '</td></tr>';
  html += '<tr><td>UserAgent</td><td>' + navigator.userAgent + '</td></tr>';

  html += '</tbody></table>';
  $('#info').html(html);
}
