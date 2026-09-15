# AbrPlayer 技术说明

本文档按当前代码实现整理，核心文件为：

- `abrplayer/index.html`
- `abrplayer/js/v3/utils.js`
- `abrplayer/js/v3/abrplayer.js`
- `abrplayer/js/v3/main.js`
- `backend/play.go`

## 运行方式

AbrPlayer 是静态 Web 播放页，由 `backend/` 托管。默认访问地址：

```text
http://127.0.0.1:8080/
```

常用 URL：

```text
/?site=studio_table1&view=fwh&quality=auto
/?site=studio_table2&view=fwv&quality=720P
/?site=studio_table1&view=fwh&quality=audio
```

页面默认不自动播放，用户需要点击播放按钮。`embedded=1&autoplay=1&preview=grid&muted=1` 用于 multipusher 的内嵌预览场景。

## URL 参数

| 参数 | 说明 |
| --- | --- |
| `site` | 支持 `studio_table1`、`studio_table2`，也可由流名推断。 |
| `view` | 支持 `fwh`、`fwv`。`studio_table1` 默认 `fwh`，`studio_table2` 默认 `fwv`。 |
| `quality` | 支持 `auto`、`1080P`、`720P`、`360P`、`audio`，内部归一化为 `auto`、`high`、`standard`、`economic`、`bottom`。 |
| `bottom` / `economic` / `standard` / `standardHevc` / `high` | 可覆盖对应档位的精确流名，multipusher 预览链接会使用这些参数。 |
| `embedded` | `1` 时进入内嵌模式。 |
| `autoplay` | 配合内嵌预览使用。 |
| `preview` | `grid` 时显示预览网格。 |
| `muted` | `1` 时静音播放。 |

## 后端接口

### `GET /api/settings/app-env`

返回当前 AbrPlayer 运行环境和外部 StatAPI base URL。播放器加载或播放前会请求该接口，并用 `baseUrl` 拼接埋点接口。

成功响应：

```json
{
  "code": 200,
  "msg": "OK",
  "data": {
    "env": "test",
    "domain": "https://videostat-test.example.com",
    "baseUrl": "https://videostat-test.example.com"
  }
}
```

默认环境：

| env | StatAPI base URL |
| --- | --- |
| `test` | `https://videostat-test.example.com` |
| `uat` | `https://videostat-uat.example.com` |
| `stag` | `https://videostat-stag.example.com` |
| `prod` | `https://videostat-prod.example.com` |

如果接口不可用或返回空域名，播放器会跳过埋点上报。当前 backend 不提供本地 `/api/stat/*` 兜底接口。

### `GET /api/play/txUrl`

返回腾讯云 WebRTC 播放地址。该接口固定请求 AbrPlayer 同源 backend，不走 StatAPI 域名。

调用方式一：按现场、视角和质量解析流名。

```text
GET /api/play/txUrl?site=studio_table1&view=fwh&quality=720P
```

调用方式二：按精确流名请求。

```text
GET /api/play/txUrl?stream=table1-fwh_standard_hevc
```

`stream` 优先级高于 `site/view/quality`。`stream` 只允许字母、数字、`-`、`_`，最大 128 字符。

质量到流名映射：

| quality | streamQuality | 请求流名 |
| --- | --- | --- |
| `auto` | `standard` | `{base}_standard`，播放器侧可根据 HEVC 支持优先切到 `{base}_standard_hevc` |
| `1080P` / `high` | `high` | `{base}` |
| `720P` / `standard` | `standard` | `{base}_standard` |
| `720P_HEVC` / `standard_hevc` / `hevc` | `standard_hevc` | `{base}_standard_hevc` |
| `360P` / `economic` | `economic` | `{base}_economic` |
| `audio` / `bottom` | `bottom` | `{base}_economic`，播放器关闭 video，仅保留 audio |

基础流名映射：

| site | view | base |
| --- | --- | --- |
| `studio_table1` | `fwh` | `table1-fwh` |
| `studio_table1` | `fwv` | `table1-fwv` |
| `studio_table2` | `fwh` | `table2-fwh` |
| `studio_table2` | `fwv` | `table2-fwv` |

成功响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "webrtc": "webrtc://play.example.com/live/table1-fwh_standard_hevc?txTime=...&txSecret=...",
    "stream": "table1-fwh_standard_hevc",
    "site": "",
    "view": "",
    "quality": "stream",
    "streamQuality": "standard_hevc",
    "txTime": "...",
    "txSecret": "..."
  }
}
```

## 页面选择器

| 控件 | 当前值 |
| --- | --- |
| Site name | `studio_table1`, `studio_table2` |
| View name | `fwh`, `fwv` |
| quality level | `auto`, `1080P`, `720P`, `360P`, `audio` |

`quality=auto` 使用浏览器侧 ABR 自动选择。其他质量为手动指定，不启用自动升降级。

## 流梯和 audio-only

以基础流名 `{siteName}` 为例：

| 档位 | 逻辑流名 | 播放请求 | 码率 | 编码 |
| --- | --- | --- | --- | --- |
| bottom | `{siteName}_audio` | `{siteName}_economic` | 128k audio 逻辑档 | 播放器关闭 video |
| economic | `{siteName}_economic` | `{siteName}_economic` | 400k | H.264 |
| standard_hevc | `{siteName}_standard_hevc` | `{siteName}_standard_hevc` | 600k | HEVC |
| standard | `{siteName}_standard` | `{siteName}_standard` | 1000k | H.264 |
| high | `{siteName}` | `{siteName}` | 2000k | H.264 |

`bottom` 是播放器内部逻辑档位。当前实现通过 `getPlaybackStreamForRequest()` 将 `_audio` 逻辑流转换为 `_economic` 播放请求，并设置 `receiveVideo=false`、`receiveAudio=true`。

## HEVC 策略

播放器使用 `MediaCapabilities.decodingInfo()` 检测 HEVC 支持。standard 质量有两个子流：

- 支持 HEVC 且未处于 HEVC backoff：优先 `{siteName}_standard_hevc`
- 不支持 HEVC 或 HEVC 回退中：使用 `{siteName}_standard`

当前 `HEVC_BACKOFF_MS = 300000`。HEVC 非网络错误会回退到 H.264 standard，并在 5 分钟内避开 HEVC；网络疑似异常则按普通降级处理，不永久禁用 HEVC。

## ABR 阈值

当前 `abrplayer/js/v3/abrplayer.js` 的关键配置：

```javascript
const CONFIG = {
  SCRIPT_VERSION: 'v1.0.3-audio360',
  ABR_SWITCH_COOLDOWN_MS: 50000,
  MONITOR_INTERVAL_MS: 2000,
  UPGRADE_PROBE_INTERVAL_MS: 12000,
  AUDIO_ONLY_RECONNECT_DELAY_MS: 8000,
  AUDIO_ONLY_RECONNECT_MAX_ATTEMPTS: 2,
  UPGRADE_CONFIRM_COUNT: 1,
  DOWNGRADE_CONFIRM_COUNT: 3,
  SEVERE_DOWNGRADE_CONFIRM_COUNT: 1,
  UPGRADE_PROBATION_MS: 15000,
  UPGRADE_BACKOFF_MS: 90000,
  HEVC_BACKOFF_MS: 300000,
  NETWORK_ERROR_GRACE_MS: 60000,
  BITRATE_DOWN_FACTOR: 0.6,
  STALL_THRESHOLD_MS: 5000
};
```

```javascript
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
```

升级确认次数为 1，降级确认次数为 3。严重降级场景使用 `SEVERE_DOWNGRADE_CONFIRM_COUNT = 1`。

## 带宽探测

主动测速只下载同源静态资源，不依赖视频编码能力：

```text
NIOES8001.jpeg
```

当前实现最多读取 120 KB，最多等待 5 秒，返回单位为 kbps。测速不可用或结果为 0 时保持当前流并记录网络异常日志，不强制切到 audio。

## 播放与切流

播放器使用腾讯云 `TCPlayer`。WebRTC 切流为 hard switch：

1. 切换前调用 `freezeLastFrame()` 把最后一帧绘制到 canvas。
2. 销毁旧播放器实例。
3. 根据目标流重新请求 WebRTC URL。
4. 创建新的 `TCPlayer` 实例。
5. 新流进入 `playing` 后移除 canvas。

auto 模式降到 bottom 或手动选择 audio 时：

- 逻辑质量标记为 `bottom`。
- 播放请求切到 `{siteName}_economic`。
- TCPlayer 配置 `receiveVideo=false`、`receiveAudio=true`。
- 页面隐藏 video，显示本地 fallback snapshot。
- 兜底 audio 流上的 fatal player error 会被抑制，避免反复降级。

## 截图兜底

1080P 或 720P 视频流进入 `playing` 后，前端会把当前视频帧保存到浏览器 `localStorage`；360P 不更新截图：

```text
localStorage key: abrplayer:fallbackSnapshot
```

只保存一张最近截图，最长边压缩到不超过 960px。切到 audio-only 时，播放器从 `localStorage` 读取 JPEG data URL 并绘制到 `snapshot-canvas`，避免 `receiveVideo=false` 时黑屏。

## 播放错误处理

当前 fatal error code：

```text
14, 1001, 1002, -2001, -2004, -2005
```

处理顺序：

1. audio-only 兜底流错误：清理错误状态并继续保底。
2. `-2004` 且流名包含下划线：尝试 legacy 流名回退。
3. HEVC 网络异常：按网络问题降级，不进入 HEVC backoff。
4. HEVC 非网络错误：回退到 H.264 standard 并进入 HEVC backoff。
5. 其他 fatal error：强制降到 bottom。

## StatAPI 上报

播放器通过 `GET /api/settings/app-env` 获取 StatAPI base URL，然后拼接：

```text
{statapiBaseUrl}/api/stat/play/start
{statapiBaseUrl}/api/stat/play/end
{statapiBaseUrl}/api/stat/lag
```

播放签名接口不走 StatAPI 域名，固定请求同源：

```text
/api/play/txUrl?stream={stream name}
```

## 控制台日志

播放成功时会输出：

```text
[Player] playing stream=..., quality=..., level=..., codec=..., resolution=..., loadTime=... ms
```

audio 统计会输出：

```text
[PlayerAudio] stream=..., muted=..., volume=..., bitrate=... kbps, stats=...
```

## 当前限制

- 切流仍是断开旧 WebRTC、重新连接新 WebRTC 的 hard switch。
- ABR 决策完全在浏览器侧完成，服务端不做播放端 ABR。
- 同源静态资源测速不能完整代表真实 WebRTC 链路。
- `bottom` 仍是播放器逻辑档位，不是当前 Tencent SRT 的独立播放流；实际 audio-only 使用 `_economic` 流并关闭视频。
