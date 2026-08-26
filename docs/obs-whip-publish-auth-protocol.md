# ppobs → mmx WHIP 推流鉴权规程

## 目标

只允许 ppobs（PPCDN 自研的 OBS 定制版，`obs32.1.2patched`）推流，且必须先经 ppcenter
验证 `appId`/`appSecret` 身份、由 ppcenter 签发短期 token 后才能推流；不再支持任何
手工配置的固定密钥或环境变量兜底密钥。校验只发生在 WHIP publish 请求上
(`POST /{path}/whip`)，不影响 WHEP 拉流。

服务端校验逻辑见 [internal/servers/webrtc/http_server.go](../internal/servers/webrtc/http_server.go)
的 `checkWHIPDeviceID`；解密实现见同目录 [whip_token.go](../internal/servers/webrtc/whip_token.go)。

## 一、整体流程

```
ppobs                          ppcenter                        mmx (Origin)
  |  POST /v1/publish/requests    |                                 |
  |  {appId, appSecret,           |                                 |
  |   streamName, deviceId}       |                                 |
  |------------------------------>|                                 |
  |                                | 校验 appId/appSecret            |
  |                                | 选择健康 Origin 节点             |
  |                                | 用 whipAuth.authKey 加密签发token|
  |  {whipUrl, bearerToken,       |                                 |
  |   expiresAt}                  |                                 |
  |<------------------------------|                                 |
  |                                                                  |
  |  POST {whipUrl}                                                 |
  |  Authorization: Bearer <bearerToken>                             |
  |----------------------------------------------------------------->|
  |                                          用 WHIP_AUTH_KEY 解密校验 |
  |                                          exp/appId/stream 是否匹配 |
  |  201 (通过) 或 403 (拒绝)                                         |
  |<-----------------------------------------------------------------|
```

1. ppobs 启动推流前，先携带自己的 `appId`/`appSecret`/`streamName`（以及自身设备
   UUID，见第三节）调用 ppcenter 的 `POST /v1/publish/requests`（见 ppcenter 仓库
   `doc/ppcenter-api-reference.zh-CN.md` 第 3 节）。
2. ppcenter 校验凭证、账户状态、选定 Origin 节点后，用 `whipAuth.authKey` 加密签发
   一个 24 小时有效期的 bearer token，随 `whipUrl`（该 Origin 节点的真实 WHIP 地址）
   一起返回。
3. ppobs 用返回的 `whipUrl` 发起 WHIP POST，`Authorization: Bearer <bearerToken>`。
4. mmx 收到后用本地 `.env` 里的 `WHIP_AUTH_KEY` 解密校验；通过则接受推流，否则
   `403` 拒绝——不区分原因（解密失败/过期/路径不符）都是同一响应，避免向未授权方
   泄露具体失败原因。

`obs32.1.2patched` 早期版本里那套“手填 Bearer Token，留空则用 `WHIP_WS_SECRET`
环境变量或硬编码默认值兜底”的方案已经完全移除，不再支持——ppobs 现在只有这一条
鉴权路径。

## 二、token 格式

- 明文 JSON：`{"uuid": "...", "appId": "...", "stream": "...", "iat": <unix秒>, "exp": <unix秒>}`。
  `appId`/`stream` 就是本次请求的 `appId`/`streamName`，也正是 mmx 收到的 WHIP
  推流路径（`{appId}/{streamName}/whip` → pathName 为 `appId/stream`）。`exp` 固定为
  `iat + 24h`（不可配置）。
- 加密：AES-256-GCM，密钥 = SHA-256(`whipAuth.authKey` 配置字符串)——用 SHA-256
  归一化是为了让部署方可以设置任意长度/格式的字符串作为密钥，而不强制要求正好
  32 字节。GCM 自带认证标签，密文一旦被篡改会直接解密失败，不需要额外的签名层。
- 线上编码：`base64.RawURLEncoding.EncodeToString(nonce || GCM密文)`，整个字符串就是
  `Authorization: Bearer <token>` 的值——不再有旧方案里 `"u"+txTime+":"+txSecret`
  那种前缀拼接格式。
- ppcenter 侧实现：`vlb/internal/utils.EncryptWHIPToken`（ppcenter 仓库
  `internal/utils/whip_token.go`）。mmx 侧实现：本仓库
  [internal/servers/webrtc/whip_token.go](../internal/servers/webrtc/whip_token.go) 的
  `decryptWHIPToken`——两边是独立实现，字段名和加密参数必须保持完全一致，改动
  一边必须同步改另一边。

## 三、ppobs 设备 UUID

ppobs 要在自己发起的 `/v1/publish/requests` 请求里带上 `deviceId` 字段，用来明确
"这是 ppobs 在推流"、而不是通用 OBS 或第三方 WHIP 客户端。格式：

```
<3位随机数字><硬盘序列号><"ppobs"><3位随机数字>
```

例如 `047 1A2B3C4D ppobs 829` → `0471A2B3C4Dppobs829`。

- 硬盘序列号：取系统盘（通常是 `C:`）的卷序列号（`GetVolumeInformationW`），格式化
  为 8 位大写十六进制。
- 前后各 3 位随机数字：每次生成时随机取，避免序列号本身在多台使用同一批次硬盘
  镜像的机器上完全一致时发生碰撞。
- 生成一次后持久化保存（ppobs 侧：`obs_module_config_path("ppobs-device-id.txt")`），
  重启后复用同一个 UUID，作为这台设备的稳定身份标识。
- 这个 UUID 会被原样封入 token 的 `uuid` claim，供 mmx/ppcenter 侧审计、异常设备
  排查使用；mmx 的 `checkWHIPDeviceID` **不**校验 `uuid` 字段本身的格式或内容，
  只校验 `appId`/`stream`/`exp`——`uuid` 是审计信息，不是鉴权凭证的一部分（真正的
  鉴权凭证是"能否用 `WHIP_AUTH_KEY` 成功解密整个 token"这件事本身）。

ppobs 侧实现见 `obs32.1.2patched` 仓库（独立仓库，不在本仓库内）的
`plugins/obs-webrtc/ppobs-identity.h`/`.cpp`。

## 八、mmx-to-mmx 转发（forwardMmx）不走 ppcenter token

`checkWHIPDeviceID` 校验的是"WHIP publish 请求"这个动作本身，而 Origin 主动把整条流
转推到另一个 mmx 节点的 `forwardMmx` 功能（见 [internal/forward/mmx.go](../internal/forward/mmx.go)、
`Path.ForwardMmx*`）底层走的也是 WHIP POST，会命中同一个 `checkWHIPDeviceID`——但
forwardMmx 是你自己运维的节点之间的基础设施流量，不是外部租户推流，没有
`appId`/`appSecret`可言，也不适合用一个 24h 就过期的短期 token。

因此 `checkWHIPDeviceID` 对这种场景单独开了一个口子：如果 token 解密失败，会再
退一步用 `.env` 里的 `MMX_FORWARD_SECRET` 做**常量时间字符串比对**，两者任一通过
即放行。用法：

- 在参与转发的每个节点（发起转发的 Origin + 接收转发的目标节点）`.env` 里配上
  **完全相同**的 `MMX_FORWARD_SECRET`。
- 在 Origin 的 path 配置里，把这个值填进 `forwardMmxToken`（或
  `forwardMmxTargets[].token`）。

`MMX_FORWARD_SECRET` 和 `WHIP_AUTH_KEY`（外部 ppobs 用）、`WHIP_WS_SECRET`（降级协议
WS 通道用）三者相互独立，**不要复用同一个值**——一旦复用，其中一个用途的密钥泄露
会连带影响另外两个。

## 四、User-Agent / 身份标识

ppobs 发出的所有 WHIP 相关 HTTP 请求（包括调用 ppcenter 的
`/v1/publish/requests`，以及 WHIP POST/DELETE 本身）都带上：

```
User-Agent: ppobs/1.0 (OBS-Studio/<版本>; <操作系统>; <locale>)
```

前缀从原来通用的 `Mozilla/5.0 ...` 改成了 `ppobs/1.0 ...`，让 ppcenter/mmx 侧的
日志、异常/滥用排查能一眼区分出这是 ppobs 客户端而不是通用 OBS 或第三方 WHIP
推流工具。实现见 `obs32.1.2patched` 仓库 `plugins/obs-webrtc/whip-utils.h` 的
`generate_user_agent()`。

## 五、mmx 侧配置

- `.env` / `bin/.env` 的 `WHIP_AUTH_KEY`：解密密钥，必须和 ppcenter 的
  `whipAuth.authKey` 配置值完全一致。**没有内置默认值**——未配置时
  `checkWHIPDeviceID` 解密必然失败，所有 WHIP publish 请求 fail-closed 返回 `403`，
  直到显式配置这个变量为止（见 [internal/conf/conf.go](../internal/conf/conf.go) 的
  `WebRTCWHIPAuthKey` 和 [.env.example](../.env.example)）。
- 这个密钥和降级协议 WS 控制通道（`ws://.../{path}/ws/whip`）用的
  `WHIP_WS_SECRET` 是**两个完全独立的密钥**，互不影响——`WHIP_WS_SECRET` 继续只
  负责降级协议自己的 WS 鉴权，改动其中一个不需要动另一个。详见
  [docs/obs-mmx-degrade-protocol.md](obs-mmx-degrade-protocol.md)。
- 密钥轮换：ppcenter 和所有 mmx Origin 节点必须同步更新 `WHIP_AUTH_KEY`，否则
  轮换窗口期内旧节点会拒绝新签发的 token（反之亦然）。由于 token TTL 只有 24
  小时，正常滚动重启节奏（先切 ppcenter，再逐个重启 mmx 节点）产生的窗口期内
  失败请求会在下一次 ppobs 重连时用新 token 自动恢复，不需要额外协调。

## 六、验证方式

```
1. 不带 Authorization 头 POST /{appId}/{stream}/whip
   → 期望 403

2. 带一个用错误的 authKey 加密出来的 token
   → 期望 403（AES-GCM 认证标签校验失败）

3. 带一个已过期（exp 已过）的合法 token
   → 期望 403

4. 带一个 appId/stream 与实际推流路径不符的合法 token（比如给 path A 签发的
   token 拿去推 path B）
   → 期望 403

5. 走完整流程：先调 ppcenter POST /v1/publish/requests 换取 token，再用返回的
   whipUrl + bearerToken 发起 WHIP POST
   → 期望 201（正常进入 SDP offer/answer 流程）
```

可以先用 curl 直接验证 mmx 侧行为（不需要起 ppobs，但需要先手工构造一个合法
token——用 ppcenter 侧的 `EncryptWHIPToken` 或等价脚本）：

```bash
curl -i -X POST "http://127.0.0.1:8889/appId/streamName/whip" \
  -H "Content-Type: application/sdp" \
  -H "Authorization: Bearer <token>" \
  --data "<offer sdp>"
```

## 七、已知限制 / 后续协调点

- 当前部署（`origin.local.yml`）没有配置 `publishUser`/`publishPass`，也没启用
  `authMethod: http` 或 `authMethod: jwt`，所以这套机制目前是 WHIP publish 唯一
  生效的鉴权路径，不会跟其他鉴权方式冲突。
- `AUTH_WEBHOOK`（`internal/auth.Manager`，`authMethod: webhook`）：确认过是从未
  实现的死配置项——mmx 代码里没有任何地方读取它，`authMethod` 的合法枚举值里
  也没有 `webhook`（只有 `internal`/`http`/`jwt`，见
  [internal/conf/auth_method.go](../internal/conf/auth_method.go)）。已从
  `.env.example` 里移除，避免继续误导部署方以为这是个可用选项。
