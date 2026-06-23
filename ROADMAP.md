# wechatbot-go 迁移路线图

本路线图记录从 [openclaw-weixin](https://github.com/Tencent/openclaw-weixin)（功能最全的 TypeScript 参考实现）向 `wechatbot-go` 迁移/补全特性的计划。

---

## 当前状态（v0.3.0）

以下详细条目保留迁移背景；当前实现状态以本节为准。

- 已完成：Phase 1.1-1.4、Phase 2.1-2.5、Phase 3.1、Phase 3.3-3.5、Phase 4.1-4.4、Phase 5.1-5.4。
- v0.3.0 额外修复：未登录/会话暂停发送返回明确错误，文本按 UTF-8 字符安全分片，QR/API 坏响应返回 decode/HTTP 错误，CDN/远程下载复用 SDK HTTP client，多账号 context/cursor 状态在登录后绑定真实账号。
- 未完成：Phase 3.2 语音 SILK → WAV 转码。

---

## Phase 1：状态持久化与健壮性（最高优先级）

这些改动直接影响生产可用性，建议先做。

### 1.1 context_token 持久化

- **来源**：`openclaw-weixin/src/messaging/inbound.ts`
- **历史现状**：Go 版本仅用 `sync.Map` 内存缓存，进程重启丢失。
- **目标**：按 `accountId:userId` 维度持久化到 `~/.wechatbot/context_tokens.json`，`Login()` 时恢复。
- **验收**：重启进程后仍能直接 `Reply()`，无需等待新消息。

### 1.2 get_updates_buf 持久化

- **来源**：`openclaw-weixin/src/storage/sync-buf.ts`
- **历史现状**：`cursor` 仅存内存。
- **目标**：保存到 `~/.wechatbot/cursor.json`，`Run()` 启动时读取，每次更新后写入。
- **验收**：重启后从上次 cursor 继续轮询，不漏消息、不重复。

### 1.3 typing_ticket 缓存

- **来源**：`openclaw-weixin/src/api/config-cache.ts`
- **历史现状**：每次 `SendTyping`/`StopTyping` 都调 `getConfig`。
- **目标**： per-user 缓存，24h TTL + 随机刷新 + 失败指数退避。
- **验收**：连续多次 `SendTyping` 只产生一次 `getConfig` 调用。

### 1.4 -14 session 过期 guard

- **来源**：`openclaw-weixin/src/api/session-guard.ts`
- **历史现状**：收到 -14 立即删除凭证并重新扫码登录。
- **目标**：收到 -14 后暂停该账号 1 小时，避免死循环；暂停期间直接 sleep。
- **验收**：模拟 -14 后不会触发无限重登录。

---

## Phase 2：协议对齐（高优先级）

补齐与上游协议细节的差异，便于后端识别和路由。

### 2.1 base_info.bot_agent

- **来源**：`openclaw-weixin/src/api/api.ts#buildBaseInfo()` / `#sanitizeBotAgent()`
- **历史现状**：Go 版本 `base_info` 只有 `channel_version`。
- **目标**：`Options` 增加 `BotAgent`，按 UA 格式校验并发送到 `base_info.bot_agent`。
- **验收**：抓包可见 `bot_agent` 字段。

### 2.2 routeTag / SKRouteTag header

- **来源**：`openclaw-weixin/src/auth/accounts.ts#loadConfigRouteTag()`
- **历史现状**：Go 版本未发送 `SKRouteTag`。
- **目标**：`Options` 增加 `RouteTag`，非空时作为 `SKRouteTag` header 发送。
- **验收**：请求头包含 `SKRouteTag`。

### 2.3 notifyStart / notifyStop

- **来源**：`openclaw-weixin/src/api/api.ts#notifyStart()` / `#notifyStop()`
- **历史现状**：Go 版本未调用。
- **目标**：`Run()` 开始时调用 `/ilink/bot/msg/notifystart`，`Stop()` 时调用 `notifystop`。
- **验收**：启动/停止时产生对应请求。

### 2.4 iLink-App-ClientVersion 自动计算

- **来源**：`openclaw-weixin/src/api/api.ts#buildClientVersion()`
- **历史现状**：Go 硬编码 `"256"`（0.1.0）。
- **目标**：从模块版本自动计算 `0x00MMNNPP`。
- **验收**：版本 `v0.2.1` 发送 `0x00020001`（513）。

### 2.5 longpolling_timeout_ms

- **来源**：`openclaw-weixin/src/monitor/monitor.ts`
- **历史现状**：Go 固定 45s，忽略服务器返回的 `longpolling_timeout_ms`。
- **目标**：动态调整下次长轮询超时。
- **验收**：服务端返回新的 timeout 后被采纳。

---

## Phase 3：消息体验（中优先级）

提升消息收发质量，大部分可参考 `wechatbot/nodejs` 已有实现。

### 3.1 Markdown 过滤

- **来源**：`openclaw-weixin/src/messaging/markdown-filter.ts`
- **参考**：`wechatbot/nodejs/src/media/markdown.ts`
- **历史现状**：Go 直接发送原始文本。
- **目标**：发送文本前可选 strip markdown，保留代码块内容，移除图片链接。
- **验收**：`![alt](url)` 被移除，`*bold*` 变为 `bold`。

### 3.2 语音 SILK → WAV 转码

- **来源**：`openclaw-weixin/src/media/silk-transcode.ts`
- **参考**：`wechatbot/nodejs/src/media/voice.ts`
- **历史现状**：Go `Download()` 返回 raw SILK bytes。
- **目标**：`Download()` 尝试把 SILK 解码为 WAV；失败时回退 SILK。
- **验收**：收到语音消息时默认拿到 `.wav` 数据。

### 3.3 富媒体分条发送

- **来源**：`openclaw-weixin/src/messaging/send.ts#sendMediaItems()`
- **历史现状**：Go 把 caption 和 media 塞进同一个 `item_list`。
- **目标**：caption 作为独立 TEXT 消息先发送，media 再单独发送。
- **验收**：发送带文字的图片时产生两条消息（或两条 sendmessage 请求）。

### 3.4 远程媒体下载

- **来源**：`openclaw-weixin/src/cdn/upload.ts#downloadRemoteImageToTemp()`
- **参考**：`wechatbot/nodejs/src/media/remote.ts`
- **历史现状**：Go `SendContent` 只接受 `[]byte`。
- **目标**：`SendContent` 支持传入 URL 字符串，SDK 自动下载后上传。
- **验收**：`SendImage("https://example.com/a.png")` 可成功发送。

### 3.5 发送失败错误通知

- **来源**：`openclaw-weixin/src/messaging/process-message.ts`
- **历史现状**：Go 只回调 `OnError`。
- **目标**：可选地给用户发送一条错误提示消息（如“媒体下载失败”）。
- **验收**：发送失败时用户收到提示。

---

## Phase 4：登录流程增强（中优先级）

### 4.1 二维码验证码（verify_code）

- **来源**：`openclaw-weixin/src/auth/login-qr.ts`
- **历史现状**：Go 未处理 `need_verifycode` / `verify_code_blocked`。
- **目标**：状态为 `need_verifycode` 时通过回调或 stdin 读取验证码继续轮询。
- **验收**：遇到需要验证码的账号能完成登录。

### 4.2 已绑定账号处理

- **来源**：`openclaw-weixin/src/auth/login-qr.ts#binded_redirect`
- **历史现状**：Go 未处理 `binded_redirect`。
- **目标**：识别该状态并视为已登录成功，使用本地已有凭证。
- **验收**：已绑定账号重新登录时不再要求扫码。

### 4.3 local_token_list

- **来源**：`openclaw-weixin/src/auth/login-qr.ts#getLocalBotTokenList()`
- **历史现状**：Go `GetQRCode` 不带历史 token。
- **目标**：请求 QR 时附带本地最近 10 个有效 token。
- **验收**：抓包可见 `local_token_list` 字段。

### 4.4 多账号隔离

- **来源**：`opensrc-weixin/src/auth/accounts.ts`
- **历史现状**：Go 单账号、单凭证文件。
- **目标**：`Options.AccountID` / `Name` 隔离凭证路径，支持多账号。
- **验收**：同一机器可登录多个微信 bot。

---

## Phase 5：扩展性（低优先级）

### 5.1 Middleware / 发送钩子

- **参考**：`wechatbot/nodejs/src/middleware/`
- **目标**：为 `OnMessage` 增加中间件链；增加 `BeforeSend` / `AfterSend` 钩子。
- **验收**：可注册 filter/rate-limit 中间件。

### 5.2 Slash 命令

- **来源**：`openclaw-weixin/src/messaging/slash-commands.ts`
- **目标**：内置 `/echo`、`/toggle-debug` 等命令，或提供扩展点。
- **验收**：发送 `/echo hello` 能直接收到回复。

### 5.3 缩略图上传

- **来源**：`openclaw-weixin/src/cdn/upload.ts`
- **目标**：图片/视频上传时自动生成缩略图并上传。
- **验收**：`getuploadurl` 请求不再传 `no_need_thumb=true`。

### 5.4 结构化日志与 Token Redaction

- **来源**：`openclaw-weixin/src/util/logger.ts` / `redact.ts`
- **目标**：替换 `fmt.Fprintf(os.Stderr, ...)`，支持 level、redact token。
- **验收**：debug 日志不泄露 `bot_token` / `context_token`。

---

## 执行建议

按 Phase 顺序推进，每个 Phase 内按编号顺序做。Phase 1 做完后项目基本具备长期运行的稳定性，后面可以慢慢补体验。
