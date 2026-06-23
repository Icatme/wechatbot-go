# wechatbot-go

微信 iLink Bot SDK for Go —— 仅维护 Go 版本。

> 本项目 fork 自 [corespeed-io/wechatbot](https://github.com/corespeed-io/wechatbot)，
> 仅保留并维护其中的 **Go SDK** 版本。原仓库还包含 Node.js、Python、Rust 等实现，
> 如果你需要其他语言版本，请访问原仓库。
>
> 核心协议与原始代码保持一致，后续会针对 Go SDK 持续做特性补全和稳定性改进。

---

## 安装

```bash
go get github.com/Icatme/wechatbot-go
```

要求 Go 1.22+，零 CGO 依赖。

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

- 原仓库：`github.com/corespeed-io/wechatbot`（多语言）
- 本仓库：`github.com/Icatme/wechatbot-go`（仅 Go）
- 许可证：MIT（保留原项目版权声明）

## 贡献

由于这是独立维护的 Go-only fork，Issues 和 PR 请提交到本仓库。

## License

[MIT](LICENSE)
