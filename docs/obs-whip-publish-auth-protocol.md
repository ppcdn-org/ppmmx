# OBS → mmx WHIP 推流鉴权规程

## 目标

防止未经授权的客户端直接向 mmx 的 WHIP publish 端点(`POST /{path}/whip`)发起推流。
要求每一次 WHIP 推流请求都携带与 `WHIP_WS_SECRET` 一致的凭证,读流(WHEP)不受影响。

服务端校验逻辑已经实现并上线,见 [internal/servers/webrtc/http_server.go](../internal/servers/webrtc/http_server.go)
的 `checkWHIPDeviceID`。这份文档描述 OBS 侧需要配合做的修改。

## 一、凭证传递方式(服务端两种都接受)

| 方式 | Header | 说明 |
|---|---|---|
| 首选(自定义) | `WHIP-Device-Id: <secret>` | 独立 header,不会跟任何路径级用户名/密码或 JWT 鉴权抢占 `Authorization` |
| 兼容(标准) | `Authorization: Bearer <secret>` | 仅当 `WHIP-Device-Id` 缺失时作为 fallback 读取,复用 OBS 内置的 "Bearer Token" 发送逻辑 |

服务端顺序:先看 `WHIP-Device-Id`;没有则看 `Authorization` 是否为 `Bearer ` 前缀,取后半段;
两者都没有,或者值跟 `WHIP_WS_SECRET` 不一致 → 返回 `401 {"error":"publish deviceID authentication failure!"}`。

这个校验**只发生在 publish 请求上**(`POST /{path}/whip`),不影响 `/{path}/whep` 的拉流请求。

## 二、密钥来源与同步

- mmx 侧:环境变量 / `bin/.env` 的 `WHIP_WS_SECRET`。**没有内置默认值**——未配置时
  `checkWHIPDeviceID` 会拿到空字符串,任何凭证都比不出相等,所有 publish 请求会 fail-closed
  返回 401,直到显式配置这个变量为止(见 [internal/conf/conf.go](../internal/conf/conf.go) 和
  [internal/servers/webrtc/http_server.go](../internal/servers/webrtc/http_server.go) 的
  `checkWHIPDeviceID`)。
- 这个密钥和降级协议 WS 控制通道(`ws://.../{path}/ws/whip`)用的是**同一个值**,
  见 [docs/obs-mmx-degrade-protocol.md](obs-mmx-degrade-protocol.md)。OBS 侧目前已经在读取这个值去连 WS,
  现在同一个值还要附加到 WHIP POST 请求上——不要引入第二处独立配置。
- 密钥轮换时,mmx 和 OBS 两侧必须同步更新,否则推流会被拒绝。

## 三、OBS 侧需要的修改

目标:每次发起 WHIP POST 推流请求时**自动**带上凭证,不依赖人工在 UI 里手动填 "Bearer Token"。

1. 在构造 WHIP POST(SDP offer)请求时,追加 header:

   ```
   Authorization: Bearer <WHIP_WS_SECRET 的值>
   ```

   推荐用这种方式而不是自定义 `WHIP-Device-Id` header,因为可以直接复用 obs-webrtc 插件已有的
   `bearer_token` 字段和发送逻辑,不需要新增 header 拼装代码。

2. 这个值必须读取自跟 WS 客户端鉴权**同一个配置来源**(同一份本地密钥配置),避免出现两处配置不同步。

3. 降级协议触发的推流重启(切换层级/码率)必须重新带上这个 header——每次重启都是全新的 WHIP
   session,凭证不会被服务端记住。

4. 只需要覆盖 publish 方向。WHEP(只读拉流)不受这个校验影响,不需要修改。

## 四、验证方式

```
1. 不带任何凭证 POST /{path}/whip
   → 期望 401 {"error":"publish deviceID authentication failure!"}

2. 带 Authorization: Bearer <正确密钥> POST /{path}/whip
   → 期望正常进入 SDP offer/answer 流程(不再是 401;后续失败应该是别的原因,比如 SDP 格式问题)

3. 带 Authorization: Bearer <错误密钥>
   → 期望仍然 401
```

可以先用 curl 直接验证服务端行为(不需要起 OBS):

```bash
curl -i -X POST "http://127.0.0.1:8889/live/<path>/whip" \
  -H "Content-Type: application/sdp" \
  -H "Authorization: Bearer <WHIP_WS_SECRET>" \
  --data "<offer sdp>"
```

## 五、已知限制 / 后续协调点

- 当前部署(`origin.local.yml`)没有配置 `publishUser`/`publishPass`,也没启用 `authMethod: http`
  或 `authMethod: jwt`,所以 `Authorization: Bearer` 目前不会跟其他鉴权方式冲突。
- 如果之后这个部署要开启基于用户名密码(Basic)或 JWT 的推流鉴权,Basic 认证跟 Bearer 不冲突,
  但 JWT 认证同样走 `Authorization: Bearer`,到时候会跟这个方案抢同一个 header。届时应该改用
  第一节里的自定义 header `WHIP-Device-Id`,不要再依赖 `Authorization` fallback。

origin-mmx 推流降级协商规程,
WHIP同播层数减少，是从高分辨率层开始减少，还是从低分辨率层开始减少？
