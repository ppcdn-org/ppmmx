# OBS 绝对时间戳协议 (obs-abs-timestamp-protocol v1) — mmx 侧实现

协议定义见 OBS 仓库（另一个独立仓库）的 `docs/obs-abs-timestamp-protocol.md`（该协议本身由 OBS 侧文档维护，mmx 只是消费方，这里不重复贴协议全文）。本文档只记录 mmx 这一侧具体做了什么。

## mmx 做了什么

只对接协议第 3 节的 WebRTC DataChannel 部分，不解析码流内嵌的 SEI/OBU 时间戳（第 2 节）——DataChannel 已经能直接拿到时间戳，不需要额外解码/解析码流。

- WHIP 推流（发布方向）建立 `PeerConnection` 时，监听 OBS 主动创建的 DataChannel（label 固定为 `"obs-timestamp"`），见 [`internal/protocols/webrtc/peer_connection.go`](../internal/protocols/webrtc/peer_connection.go) 的 `OnInboundDataChannel` 字段和 [`internal/servers/webrtc/session.go`](../internal/servers/webrtc/session.go) 的 `onInboundDataChannel`。
- mmx 不创建这个 channel，也不需要服务端应答——OBS 侧如果发现 mmx 没接这个 channel，发送会静默失败，不影响正常推流（协议原文已经这么设计）。
- 只解析 label 为 `"obs-timestamp"` 的 channel；其余 DataChannel（如果有）忽略。
- 收到消息后按协议格式解析（`internal/protocols/webrtc/obs_timestamp.go` 的 `ParseOBSTimestampMessage`）：
  ```json
  {"frame_no": 1234, "timestamp": 1733500000123, "rid": "0"}
  ```
- 用 `timestamp`（OBS 侧 NTP 校准后的发送时刻，UTC 毫秒）和 mmx 收到这条 DataChannel 消息时的本地时钟相减，得到"OBS 采集/发送 → mmx 接收"这一段的延迟，单位毫秒。**这不是端到端播放延迟**（不含 mmx→播放器这一段，也不含 WHEP 侧的 jitter buffer），只是推流上行链路的延迟。
- 该延迟值存在 WHIP 发布 session 上，通过 `/v3/webrtcsessions/list`（Control API）的 `obsTimestampLatencyMs` 字段暴露（`internal/defs/api_webrtc.go`）。未收到过 `obs-timestamp` 消息的会话（例如推流端不是这个补丁版 OBS，或者还没发送过）该字段为 `null`。
- 每次收到新消息会覆盖上一次的值，不做滑动平均/历史记录——需要趋势的话由调用方自己轮询这个字段采样。
- 目前**只取最新一层**的延迟（同播多层场景下，无论哪个 `rid` 的消息到达都会覆盖同一个字段），不按 `rid` 拆分成每层各自的延迟。如果后续需要分层延迟，需要把 `obsTimestampLatencyMs` 从单个 `*float64` 改成按 `rid` 索引的 map——目前没有这个需求，先保持简单。

## 不做什么

- 不解析码流内嵌的 SEI（H.264/H.265）/OBU（AV1）时间戳（协议第 2 节）——DataChannel 已经是更直接的信号源，没必要再多解一次码流。
- 不做任何基于这个延迟值的自动化决策（不触发降级、不做告警）——纯只读暴露给调用方，决策逻辑不在 mmx 职责范围内（对照降级协议 [`docs/obs-mmx-degrade-protocol.md`](obs-mmx-degrade-protocol.md)，那套是丢包驱动的独立状态机，和这里的时间戳延迟无关）。
- 不持久化历史值——进程重启、WHIP session 重连都会清零。
