# CDN Ingest Thread

移植自早期内部版本 `comm/tasks/ingestor.go` 的 `IngestMainThread`,
简化后接入到 mmx (`internal/ingest`)。用途:从外部 CDN 拉流,推到 mmx 自己的本地
RTMP 监听端口再生,让这条流像任何普通推流一样被本地的录像/分发/白名单逻辑处理。

**按需启停,不是进程启动就一直拉。** 只有 `POST /api/split-rec` 的"录像开始"请求
(`gc` 为空)传进来、且对应 path 当前没有任何人在推流时,才会真的去拉;"录像结束"
请求(`gc` 非空)传进来时,如果这条 path 是被 ingest 拉起来的,会跟着停掉。这一点
和 v120 的 `StartGameStream`/`StopGameStream`(同样挂在 `/api/split-rec` 上)是
同一个触发点,只是 mmx 这边的 split-rec 协议本身比 v120 简单(没有单独的
table-only 分支,`game` 必填)。

## 开关

两个都是 yml 字段(没有环境变量),缺省值见 `internal/conf/conf.go` 的 `setDefaults`:

- `ingestThreadEnable`:**缺省为 false**,按节点自己 opt-in,想要 ingest 的节点
  (比如 `record.local.yml`)要显式写 `ingestThreadEnable: true`。
- `ingestSources` (字符串数组),缺省值是:

  ```yaml
  ingestSources: ["tencent:rtmp://play.example.com/live/demo-stream"]
  ```

  两个条件都满足(开关为 true 且列表非空),进程启动时才会去解析这份列表、建好
  "path → 源" 的映射表——注意是**只解析,不拉流**,真正开始拉由下面的触发方式决定。

每一项格式是 `"{cdnName}:{rtmpUrl}"`:

```yaml
ingestSources:
  - "tencent:rtmp://play.example.com/live/demo-stream"   # 自动附加签名
  - "other:rtmp://cdn.example.com/live/table1-fwv"         # 原样使用，不加签名
```

`ingestSources`/`ingestThreadEnable` 只在进程启动时读取一次;运行中改这两个字段
需要重启进程才生效(不支持热重载)。

## 触发方式:POST /api/split-rec

`internal/recording/split.go` 的 `SplitRecHandler.execute` 是唯一的触发点:

1. **录像开始**(请求体 `gc` 为空):先按 `table` 解析出**一组** path——若
   `site_stream_configs` 里为该 table 配置了多个 view(比如 `fwh`/`fwv`),这里
   就是多条 path,每一条都会独立走下面的检查/ingest 触发流程(详见
   [recorder-api.md 第五节](recorder-api.md#五录像分段接口-post-apisplit-rec));
   若没有任何配置,才回退到旧的单路径规则(`live/<table>-fwh`,或
   `SetTablePathMapping` 里配的别名)。对每一条 path,查它现在是否已经有人在
   推流(`pathFinder.FindPath` / 录像任务表)。
   - 已经有(不管是 OBS 直接推流还是别的方式)→ 直接用,完全不碰 ingest。
   - 没有,且这条 path 在 `ingestSources` 里配了源 → 调用
     `ingest.Manager.StartByPath(path)` 把它拉起来,然后每 200ms 轮询一次,最多
     等 **10 秒**(`waitForIngestPath`)看它有没有真的上线(ffmpeg 连上源、mmx
     接受了这路 RTMP 推流)。10 秒内没上线,或者这条 path 根本没配 ingest 源
     (`StartByPath` 直接返回错误,不会傻等)→ 这一条 path 被跳过(只打
     WARN 日志),不影响该 table 下其他 path 正常开局;只有**全部** path 都
     失败时,整个请求才返回 `start round: no path found for table "<table>"`
     (HTTP 500)。
2. **录像结束**(请求体 `gc` 非空):正常走 `SplitRecording` 切分/改名录像文件,
   之后**无条件**调用 `ingest.Manager.StopByPath(path)`——如果这条 path 不是
   ingest 拉起来的(比如 OBS 直推),`StopByPath` 是个空操作,不会误伤。
   录像结束**不会**触发 ingest 启动(没有意义:没开始过的录像不需要收尾)。

一条 path 同一时刻只能被一个 ingest worker 占着;`StopByPath` 之后立刻可以对同一
个 path 再 `StartByPath`,不需要等上一个 ffmpeg 进程真正退出(旧连接还没断时,
mmx 会拒绝新连接的推流,ffmpeg 的退避重试会在旧连接断开后自动接上)。

## 和 forwardTencent 互斥

`ingestThreadEnable: true` 时,加载配置阶段(`internal/conf/path.go` 的
`Path.validate`)会把每一条 path 的 `forwardTencent` 强制改成 `false`,并打一条
WARN 日志,不是直接报错拒绝启动。原因:ingest 拉回来的流(`cdnName` 是
`tencent` 时)本来就来自腾讯云,如果同一个 path 又开着 `forwardTencent`,会把
这条流原样转推回腾讯云,形成"拉回来又推出去"的死循环——这不是假设,是真实复现过
的:`origin.local.yml` 之前 `ingestThreadEnable`/`forwardTencent` 都是 true,实测
ffmpeg 真的连上 `play.example.com` 拉到流之后,立刻被转推逻辑重新推回腾讯云,
对方报 `errcode:-10016 Audio Negotiate failed`。

## 行为细节

对每一条 `ingestSources`:

1. 按第一个 `:` 拆成 `cdnName` 和 `rtmpUrl` 两段(`rtmpUrl` 本身的 `://` 不受影响,
   只切第一刀)。拆不出来(没有冒号,或冒号后面是空的)就跳过并打 WARN 日志。
2. 取 `rtmpUrl` 最后一段路径作为本地流名 `<name>`(去掉 query string/fragment),
   比如 `rtmp://cdn.example.com/live/demo-stream` → `demo-stream`,本地路径就是
   `live/<name>`。取不到(比如 URL 里压根没有路径)也跳过并打 WARN。
3. `StartByPath("live/<name>")` 被调用时(见上面"触发方式"),`cdnName` 是
   `tencent` 的话,每次真正发起拉流前都会现算一遍签名并追加到 `rtmpUrl` 后面:

   ```
   txTime   = hex(unix(now + 5min))
   txSecret = md5(TX_SECRET_KEY_BACK + <name> + txTime)
   ```

   和 `internal/admin/admin.go` 的 `signedPlayParams`(给 `play.example.com`
   播放链接签名用的)以及 `internal/forward/tencent.go` 推流那边用的是同一套
   腾讯云防盗链公式,只是各自独立实现了一份(仓库里这个公式本来就没有共享成一个
   函数,这次跟着这个先例走)。key 来自 `bin/.env` 的 `TX_SECRET_KEY_BACK`——没配
   的话会在启动时打一条 WARN,拉流大概率会被拒。5 分钟有效期足够覆盖一次 ffmpeg
   握手,因为每次重连都会重新签一遍,不依赖它长期有效。
   `cdnName` 不是 `tencent` 就原样使用 `rtmpUrl`,不做任何改写——其他 CDN 需要的
   鉴权参数得自己先拼进 URL 里。
4. 起一个独立的 ffmpeg 子进程:

   ```
   ffmpeg -loglevel fatal -i <签名后的 rtmpUrl> -c copy -f flv rtmp://127.0.0.1:<rtmpAddress 端口>/live/<name>
   ```

   `-c copy` 不转码,只做协议/封装转换,尽量省 CPU。目标地址固定用本机
   `rtmpAddress`(见 `origin.local.yml` 的 `rtmpAddress: :1935`),要求 `rtmp: true`
   已开启,否则整个 ingest 子系统在启动时就会打一条 WARN 然后完全不初始化(连
   `ingestSources` 都不会解析),不会硬失败导致进程起不来。
5. ffmpeg 退出后按运行时长做退避重试(录像结束触发的 `StopByPath` 例外——那是主动
   取消,不会重试):
   - 这次运行 ≥ 30s,视为曾经正常过 → 下次重试间隔重置回 1s。
   - 这次运行 < 30s(反复起不来),重试间隔 +2s,封顶 60s。
   - 每次重试都会回到第 3 步重新签名,不会用一份过期的签名反复重试。
6. 进程整体关闭时(`newConf == nil`,即最终 shutdown,不是配置热重载的那种局部
   关闭),`internal/core/core.go` 会调用 `ingest.Manager.Close()`,停掉所有当前
   还在跑的 worker 并等 ffmpeg 子进程退出。

## 和 v120 参考实现的差异(有意简化,没有照抄)

v120 的 `comm/tasks/ingestor.go` 通过 `/api/split-rec` 触发 `StartGameStream`/
`StopGameStream` 这一层(按需拉流/停止拉流)已经照着搬过来了。**没有**移植的是:

- Nacos 热更新 `InputStreamList` 后动态增删 ingest(`ACT_RELOAD_INGEST` 等消息)。
- v120 那套 table/game 三层分支(`game` 可以为空、`gc` 可以为空,组合出四种
  语义)——mmx 的 split-rec 本来就只有两态(`gc` 空/非空,`game` 必填),不需要
  额外照搬 v120 更复杂的状态机。
- 通过 `GetTableStreamView` 解析 `table`(用于旧的 `recordApi` 的 `tableId`)——
  当前 mmx 的录像走 `record: yes` + `/api/split-rec`,不需要这个字段,所以只保留
  了派生本地流名这一半逻辑。

如果之后真的需要运行中动态增删源,再在这个基础上加,不要反过来假设这次已经支持。

## 源地址从哪来

`ingestSources` 里 `{rtmpUrl}` 部分要求是不带签名的裸地址(`cdnName` 是
`tencent` 时,签名由 mmx 自己现算现加,不要在 `rtmpUrl` 里预先带上
`txTime`/`txSecret`,不然会被追加成两份)。`cdnName` 不是 `tencent` 的情况下,
`rtmpUrl` 会原样使用,那种 CDN 需要的任何鉴权参数就得自己先拼进 URL 里再填。
