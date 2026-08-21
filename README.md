# mmx

mmx 是基于 [MediaMTX](https://github.com/bluenviron/mediamtx) v1.19.1 二次开发的直播媒体服务器,面向"OBS 同播推流 → 自适应码率播放 → 视频云备份分发"这一具体业务场景。保留了 MediaMTX 原有的多协议转发能力,在此之上新增了 WebRTC Simulcast ABR、Origin/Edge 分层组网、腾讯云转推、按需录像控制、以及一套配套的管理后台 + 播放器。

## 这是什么

- **底座**:MediaMTX v1.19.1(纯 Go、零依赖的媒体路由服务器),支持 RTSP、RTMP、HLS、SRT、WebRTC、Media-over-QUIC 之间的协议互转、录像、回放、鉴权、Hook、Prometheus 指标等全部原生能力保持不变。
- **新增能力**:见下方「核心特性」。这些都是在 mmx 项目周期内针对具体需求(OBS 同播直播 + 弱网自适应 + 腾讯云备份 + 按房间录像控制)开发的,不属于上游 MediaMTX。

## 核心特性

### WebRTC Simulcast 自适应码率(ABR)

- 接受 OBS 原生 WHIP Simulcast(最多 4 层 H264)或 WebRTC Multitrack 推流,单一 WHEP 出流,由服务端 `TrackSelector` 按关键帧边界在各层间无缝切换。
- 播放端(`mmxplayer`)内置 ABR 引擎:根据实时 FPS/带宽自动升降级,网络恶劣时可降级到纯音频(暂停视频省带宽),网络恢复后自动探测升级。
- 播放端与服务端之间有一条独立的 WebSocket 控制通道(`/ws/control`),消息类型包括 `TRACKS_INFO`、`SELECT_LAYER`/`LAYER_SWITCHED`、`SET_MEDIA_STATE`/`MEDIA_STATE`(音视频独立暂停/恢复)、`LATENCY_REPORT`、`PING`/`PONG`。
- 两个关键的"减少等待"优化:
  - 新观众首次拉流时,立即向 publisher 请求一个关键帧(IDR),把首屏等待时间从"等下一个 GOP"降到接近即时。
  - 从暂停视频恢复播放时同样主动请求关键帧,且客户端会等关键帧真正解码出来后才开始评估 FPS,避免把"还没出画面"的静默期或半截采样窗口误判成卡顿而反复抖动。
- 详见 [doc/abr-protocol.md](../doc/abr-protocol.md) 和 [doc/mmx-abr-hld.md](../doc/mmx-abr-hld.md)。

### Origin / Edge 分层组网

- **Origin(源站)**:接受 OBS 推流(WHIP/RTMP/SRT),是唯一的"源头真相"。
- **Edge(边缘节点)**:不接受推流,按需(`sourceOnDemand`)向 Origin 发起 WHEP 回源拉流,再分发给观众,起到减轻源站压力、就近分发的作用。
- Origin 与 Edge 可以部署在同一台机器(不同端口)或不同机器上,互不冲突。示例配置见 `bin/conf/origin.local.yml`、`bin/conf/edge.local.yml`。
- 详见 [doc/mmx-deployment.md](../doc/mmx-deployment.md)(容量规划)与 [doc/test/origin-edge-test-plan.md](../doc/test/origin-edge-test-plan.md)。

### 转推腾讯云直播(WHIP)

- Origin 节点可以把接收到的直播流,按配置文件驱动地转推一份到腾讯云视频直播,作为额外的分发/备份通道。
- 每个 Simulcast 视频层各自转推为腾讯云的一路独立 WHIP 推流;鉴权密钥(`TX_SECRET_KEY`)只能通过环境变量或 `.env` 提供,不允许写入 YAML。
- 服务启动时会在日志里打印转推配置摘要(是否启用、域名/App、密钥是否已配置、哪些 path 启用了转推),便于排查配置遗漏。
- 详见 [doc/forward-tencent-design.md](../doc/forward-tencent-design.md)。

### 按需录像控制 API

- `POST /api/split-rec`:供上游业务系统(如 game-server)调用,按桌台/房间触发录像分段或启停,支持 `simple`(MD5)和 `advance`(HMAC-SHA256,需 `SPLIT_REC_SECRET`)两种鉴权模式。
- 详见 [doc/api/api-split-rec.md](../doc/api/api-split-rec.md)。

### 管理后台 + 双播放器

- 内置管理后台(`internal/admin`),一对一节点管理(不是多租户面板):节点状态、路径/会话列表、配置查看、播放域名与 videostat 上报环境配置、录像下载等。入口 `http://<host>:<adminPort>/dashboard`。
- 两套播放器页面,均由管理后台托管:
  - **`/simulcast/`**:主力播放器,走 WHEP + 上述 ABR 引擎。
  - **`/multitrack/`**:备用播放器,面向腾讯云 WebRTC 多码率硬切流。
  - `GET /api/playUri` 会同时给出 mmx 自身的 WHEP 地址(main)和腾讯云的签名地址(backup),播放器默认优先用 simulcast 播放 main 地址。

## 目录结构

```
mmx-v1.19.1/
├── internal/
│   ├── core/            # 路径生命周期、核心调度(mediamtx 原有 + Tencent 转推/录像挂载点)
│   ├── forward/          # 腾讯云 WHIP 转推(签名、多层拆分、客户端）
│   ├── admin/            # 管理后台 + mmxplayer(simulcast/multitrack）静态资源
│   ├── protocols/webrtc/  # WHIP/WHEP、TrackSelector（ABR 层选择器）、MediaState（暂停/恢复）
│   └── servers/           # 各协议 server（rtsp/rtmp/hls/srt/webrtc/moq）
├── bin/conf/              # origin.local.yml / edge.local.yml 等本地配置示例
├── build.bat              # Windows 构建脚本
└── ...                    # 其余目录（api/、pkg/、docker/、scripts/ 等）为 mediamtx 原生结构
```

## 快速开始

### 构建

```bash
build.bat
```

会自动从 `internal/core/VERSION` 读取版本号(也可传参覆盖,如 `build.bat v1.20.0`),产出 `bin\mmx.exe`。

### 配置必需的密钥(环境变量 / `.env`)

以下密钥**只能**通过环境变量或 `bin/.env` 提供,写入 YAML 不会生效(会被启动时的读取逻辑覆盖):

| 变量名 | 用途 | 必需条件 |
| --- | --- | --- |
| `MMXADMIN_PASSWORD` | 管理后台首次启动的管理员密码 | 可选,管理后台首次启动时使用;未设置时会自动生成随机密码并打印到日志(之后密码存入本地 sqlite,不再依赖此变量) |
| `TX_SECRET_KEY` | 腾讯云 WHIP 转推防盗链签名密钥 | `tencentWHIPEnable: true` 时必需 |
| `TX_SECRET_KEY_BACK` | 腾讯云播放地址(backup play URI)签名密钥 | 使用 `/api/playUri` 的 backup 地址时必需 |
| `SPLIT_REC_SECRET` | `/api/split-rec` 的 HMAC-SHA256 鉴权密钥 | `splitRecAuthMode: advance` 时必需 |

### 运行

```bash
cd bin
mmx.exe conf\origin.local.yml
```

Edge 节点同理,指向 `conf\edge.local.yml`。启动日志会列出各协议监听端口、admin 后台地址,以及(如启用)腾讯云转推的配置摘要。

## 开发环境说明

- 本仓库长期在 Windows 上开发,`go test ./...` 会有一批测试因为 Windows/Unix 平台差异失败(Unix domain socket、符号链接、Windows 非法文件名字符等),这些在 Linux 部署环境下不存在,属于已知的开发环境噪音,不代表功能缺陷。

## 许可证

延续上游 MediaMTX 的 MIT License,见 [LICENSE](LICENSE)。
