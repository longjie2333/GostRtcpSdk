# GostRtcpSdk

轻量 Go SDK：连接官方 GOST 的 `relay+tls` 服务端，把远端 TCP 端口转发到本机可达的服务。**只包含客户端**，公开 API 为 `Config` 和阻塞运行的 `Run`；取消 `context` 会关闭会话、释放远端监听并等待转发任务退出。

基线：GOST v3.3.0 / go-gost/x v0.16.0，要求 Go 1.23+。只支持 TCP、单个 Relay+TLS 节点与目标，不包含服务端或通用代理框架。

## 在其他 Go 项目使用

当前是本地 Git 仓库，尚未发布到远程。`example.com/gostrtcpsdk` 是本地 module 标识，不是已发布下载地址。在使用方的 `go.mod` 中加入：

```go
require example.com/gostrtcpsdk v0.0.0
replace example.com/gostrtcpsdk => ../GostRtcpSdk
```

路径相对于使用方的 `go.mod`，按实际目录调整；也可使用 `C:/.Projects/GostRtcpSdk` 绝对路径，然后运行 `go mod tidy`。程序示例：

```go
package main

import (
    "context"
    "errors"
    "log"
    "net/url"
    "os"
    "os/signal"

    rtcp "example.com/gostrtcpsdk/src"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
    defer stop()
    cfg := rtcp.Config{
        Server: "192.168.1.2:1080",
        Bind:   "0.0.0.0:8080",
        User:   url.UserPassword("user", "password"),
    }
    if err := rtcp.Run(ctx, cfg, "127.0.0.1:80");
        err != nil && !errors.Is(err, context.Canceled) {
        log.Fatal(err)
    }
}
```

公网端继续运行官方程序，例如：

```text
gost -L "relay+tls://user:password@:1080?bind=true"
```

访客访问公网端 `8080`，SDK 从运行所在机器连接 `127.0.0.1:80`。上例地址仅用于说明。

- [完整配置、参数及生命周期说明](docs/configuration.md)
- [已保留的源版本行为和限制](docs/behavior.md)
- [可运行的代码示例](src/examples/basic/main.go)：从环境变量读取配置，`go run ./src/examples/basic`

**已知行为：**源版本的空主机绑定缺陷被保留，请显式写 `0.0.0.0:8080`，不要写 `:8080`。`TLS=nil` 默认不验证服务端身份；需要验证时提供 `Config.TLS`。复制与半关闭的已知边界详见行为说明。

## 构建与检查

```text
go build ./...
go vet ./...
go test -race -count=1 -timeout=90s ./...
```

完整检查（PowerShell 7+）还要求官方 GOST v3.3.0 二进制：

```powershell
pwsh -File scripts/check.ps1 -GostBinary C:/tools/gost.exe
```

仅单元检查可使用 `-UnitOnly`，它不代表官方互通检查通过。完整检查会验证官方二进制版本，并拒绝测试 SKIP。官方测试只访问本机回环地址。CI 定义见 [.github/workflows/checks.yml](.github/workflows/checks.yml)；尚未在远程平台运行。

## 工程与版本管理

源码与测试在 `src/`，示例在 `src/examples/`，参数文档在 `docs/`。不需要上层 demo、report 或旧 rtcp 目录才能构建。

`main` 是唯一长期开发主线；短期分支、独立 PR、审查与检查、Squash 合入，以及不可变版本 Tag 的规则见 [AGENTS.md](AGENTS.md)。当前没有配置远程仓库，不声称已启用平台分支保护或完成远程 PR。首次建库提交只建立已经验证的 SDK 工程基线，尚未创建正式发布 Tag。

## 开源声明

本项目使用 [MIT License](LICENSE)。核心代码精简自 [go-gost/x v0.16.0](https://github.com/go-gost/x/tree/14fed91c6245289e2e92ea60a97332a62c90fad0)，保留原作者版权，来源映射见 [NOTICE](NOTICE)。

| 依赖 | 版本 | 用途 | 许可 |
|---|---|---|---|
| [go-gost/relay](https://github.com/go-gost/relay) | v0.7.0 | Relay 请求、响应及 Feature 编解码 | [MIT](THIRD_PARTY_LICENSES/relay.txt) |
| [xtaci/smux](https://github.com/xtaci/smux) | v1.5.31 | TCP 上的多路复用、保活与流控 | [MIT](THIRD_PARTY_LICENSES/smux.txt) |

官方 GOST 程序仅用作互通测试对端，不作为 SDK 运行依赖打包。本工程与上述项目无官方隶属关系。
