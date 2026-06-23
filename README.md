# wechatbot-go

一个专注 Go 语言的微信 iLink Bot SDK，从多语言仓库中独立出来单独维护。

本项目基于 [corespeed-io/wechatbot](https://github.com/corespeed-io/wechatbot) 的 Go SDK 部分，但已拆分
为独立的 Go-only 仓库并持续演进。原仓库同时维护 Node.js、Python、Rust 等版本，而这里只保留和
改进 Go 实现：更完整的协议对齐、状态持久化、登录流程增强以及可扩展的消息处理钩子。

如果你需要一个纯 Go、长期维护、面向实际运行场景的微信 Bot SDK，本仓库会更合适。

---

## 安装

```bash
go get github.com/Icatme/wechatbot-go
```

要求 Go 1.25+，零 CGO 依赖。

## 快速开始

```go
package main

import (
    "context"
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

    bot.OnMessage(func(msg *wechatbot.IncomingMessage) {
        _ = bot.Reply(ctx, msg, fmt.Sprintf("Echo: %s", msg.Text))
    })

    _ = bot.Run(ctx)
}
```

更多示例见 [`examples/`](examples/)。

## 功能特性

- 扫码登录 + 凭证持久化
- 长轮询接收消息
- 文本 / 图片 / 文件 / 视频 / 语音 收发
- CDN 上传下载与 AES-128-ECB 解密
- context_token 自动管理
- 输入状态指示器
- 会话过期（`-14`）自动恢复

## 文档

- [API 协议参考](docs/protocol.md)
- [架构与 SDK 对比](docs/architecture.md)（来自原仓库，Go 部分仍适用）

## 与原仓库的关系

- 原仓库：`github.com/corespeed-io/wechatbot`（多语言 SDK 集合）
- 本仓库：`github.com/Icatme/wechatbot-go`（独立维护的 Go-only 分支）
- 协议与核心实现源自原仓库的 Go 部分，但已不再同步合并
- 许可证：MIT（保留原项目版权声明）

## 贡献

由于这是独立维护的 Go-only fork，Issues 和 PR 请提交到本仓库。

## License

[MIT](LICENSE)
