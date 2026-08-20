# wechatbot-go

wechatbot-go 是微信 iLink Bot API 的 Go SDK。

代码最初来源于 [corespeed-io/wechatbot](https://github.com/corespeed-io/wechatbot) 的 Go SDK 部分，
协议行为也参考 [Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin)。当前仓库作为
独立 Go module 维护，不与上述仓库自动同步。

项目范围包括登录、消息收发、媒体传输、投递状态和会话状态管理，不包含 Agent 推理或编排运行时。

---

## 安装

```bash
go get github.com/Icatme/wechatbot-go
```

要求 Go 1.25+，零 CGO 依赖。

## 最近更新（v0.4.1）

`v0.4.1` 是兼容性补丁版本：

- API、CDN、二维码、远程媒体和日志错误统一脱敏，覆盖签名 URL、认证信息、嵌套或畸形载荷，并限制上游错误消息长度。
- 出站 wire 消息及引用文本中的不安全裸 `<` 会归一化为全角字符；明确允许的少量 HTML 标签和行级 fenced code 保持不变。
- 出站归一化基于请求私有副本执行，避免修改调用方模板，并支持安全的并发复用。
- 将根包的大型实现按登录、会话、轮询、内容、媒体、消息、Handler 和日志职责拆分为同包文件，公共 API 与行为保持不变。

完整说明见 [v0.4.1 Release](https://github.com/Icatme/wechatbot-go/releases/tag/v0.4.1)。

### v0.4.0

`v0.4.0` 主要调整投递与会话语义：

- 增加持久化 replay 去重、原子 cursor/context/replay 状态以及 malformed message fail-closed。
- Handler 改为接收 `context.Context`，并通过 Ack / Retry / Drop 明确返回投递结果。
- 同一对端用户串行处理，不同用户在有界并发下处理；cursor 只推进到连续完成的批次前缀。
- 增加 Agent `run_id` / `session_id` / `group_id`、工具调用项类型 11/12，以及稳定的出站 `client_id`。
- API 错误可通过 `errors.As` 获取 HTTP status、`ret` 和 `errcode`。
- 会话过期后不再自动复用旧 token；调用方显式调用 `Reauthenticate(ctx)`，持久化 marker 保证进程重启后仍保持 fail-closed。

从 `v0.3.x` 迁移时需要注意：

- `Bot.OnMessage(func(*IncomingMessage))` 改为 `Bot.Handle(MessageHandler)`；Handler 需要返回 `MessageResult`。
- `LifecycleHooks.BeforeSend` 的 payload 从 `*SendContent` 改为 `*SendRequest`，并增加 `AfterSend`。
- 收到 `ErrReauthRequired` 后，由应用决定何时发起二维码交互并调用 `Reauthenticate(ctx)`。

完整说明见 [v0.4.0 Release](https://github.com/Icatme/wechatbot-go/releases/tag/v0.4.0)。

## 快速开始

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    "github.com/Icatme/wechatbot-go"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    bot := wechatbot.New(wechatbot.Options{
        OnQRURL: func(url string) { fmt.Println("请扫码:", url) },
    })

    if _, err := bot.Login(ctx, false); err != nil {
        fmt.Fprintln(os.Stderr, "登录失败:", err)
        os.Exit(1)
    }

    bot.Handle(wechatbot.MessageHandlerFunc(func(ctx context.Context, msg *wechatbot.IncomingMessage) wechatbot.MessageResult {
        if err := bot.Reply(ctx, msg, fmt.Sprintf("Echo: %s", msg.Text)); err != nil {
            return wechatbot.RetryMessage(err)
        }
        return wechatbot.AckMessage()
    }))

    if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        fmt.Fprintln(os.Stderr, "运行结束:", err)
    }
}
```

更多示例见 [`examples/`](examples/)。

## 功能范围

- 扫码登录 + 凭证持久化
- 长轮询接收消息
- 轮询与消息处理解耦；同一用户严格有序，不同用户有界并发
- Handler 接收运行上下文，并以 Ack / Retry / Drop 明确控制投递结果
- Agent `run_id` / `session_id` / `group_id` 解析与工具调用项类型（11 / 12）
- 单项出站消息支持调用方复用 `client_id`、返回发送身份并发送工具调用进度
- 按会话持久化 `message_id` / `client_id` / `seq` 全部身份别名，避免跨用户误判重放
- 消息解码失败时默认停止消费并保留 cursor，避免静默丢消息
- 文本 / 图片 / 文件 / 视频 / 语音 收发
- CDN 上传下载与 AES-128-ECB 解密
- context_token 自动管理
- 输入状态指示器
- 会话过期（`-14`）进入终止态，并由调用方显式重新认证

Handler 返回 `RetryMessage(err)` 时，当前消息的重放键与所在批次游标不会提交，`Run` 会携带该错误退出；
返回 `DropMessage(reason)` 会明确消费该消息。`Stop` 和传给 `Run` 的 context 会传播到 Handler，Handler 应及时响应取消。

默认最多同时处理 4 个用户，可通过 `Options.MaxConcurrentHandlers` 调整（最大 256，设为 `1` 可全局串行）。
同一个 Handler、Middleware 与 `AfterReceive` Hook 可能被不同用户并发调用，因此实现必须并发安全。全局轮询游标只会
推进到连续完成的批次前缀；缺少 `message_id`、`client_id` 和 `seq` 的消息仍属于 at-least-once 投递。
重放键按对端用户隔离，不同会话可安全复用相同的 `message_id`、`client_id` 或 `seq`。

任一已认证接口返回 `ret: -14` 或 `errcode: -14` 时，SDK 会清除失效凭证、配置缓存与
`context_token`，取消正在运行的轮询，并返回可由 `errors.Is(err, ErrReauthRequired)` 识别的错误。
它不会自动弹出二维码或再次使用旧 token；应用确认交互时机后调用 `Reauthenticate(ctx)`。显式重认证也会先
封锁当前健康会话，且旧 Run/Handler 不能使用新凭证发送或复活旧 `context_token`；已经明确 Ack/Drop 的
投递仍按原语义提交。需要启动新认证流程的并发或回调内重入请求会返回
`ErrLoginInProgress`；属于旧会话的后续操作返回 `ErrSessionChanged`，不表示当前会话仍需扫码。
失效转换会先持久化 `<CredPath>.reauth.json` marker；进程重启后，普通 `Login(false)` 仍会零网络请求地返回
`ErrReauthRequired`。只有 fresh credentials 已持久化且所有记录的 `context_token` 已清空后，marker 才会被移除。
迁移自定义状态目录时应将 credentials 与该 sidecar 作为同一状态集；marker 活跃期间不应直接降级到不识别它的 SDK 版本。

## 文档

- [API 协议参考](docs/protocol.md)
- [架构说明](docs/architecture.md)

## 来源与同步范围

- 初始 Go 实现来源：`github.com/corespeed-io/wechatbot`
- 协议参考：`github.com/Tencent/openclaw-weixin`
- 本仓库独立维护，变更不会自动与上述仓库双向同步
- 许可证：MIT（保留原项目版权声明）

## 贡献

本仓库相关的 Issues 和 PR 可直接提交到当前仓库。

## License

[MIT](LICENSE)
