# GostRtcpSdk

轻量 Go SDK：连接官方 GOST 的 `relay+tls` 服务端，把远端 TCP 连接交给本地程序。**只包含客户端**，提供两种入口：

- `Listen(ctx, cfg)` 返回标准 `net.Listener`，直接交付业务连接，本地无需监听端口。适用于 Gin、`net/http` 和自行处理 TCP 连接的服务。
- `NewClient(cfg, target)` 创建目标转发实例，`Client.Run` 持续运行，`Client.UpdateTarget` 热更新默认 TCP 目标。

基线：GOST v3.3.0 / go-gost/x v0.16.0，要求 Go 1.23+。每个入口连接单个 Relay+TLS 节点，只支持 TCP，不包含公网服务端或通用代理框架。

## 在其他 Go 项目使用

当前是本地 Git 仓库，尚未发布到远程。`example.com/gostrtcpsdk` 是本地 module 标识，不是已发布下载地址。在使用方的 `go.mod` 中加入：

```go
require example.com/gostrtcpsdk v0.0.0
replace example.com/gostrtcpsdk => ../GostRtcpSdk
```

路径相对于使用方的 `go.mod`，按实际目录调整；也可使用 `C:/.Projects/GostRtcpSdk` 绝对路径，然后运行 `go mod tidy`。

## 无本地监听端口的业务入口

宿主已经有 `http.Handler`（例如 Gin 的 `*gin.Engine` 或 JmptJwxtAPI 的 `*httpapi.Server`）时，直接交给标准 HTTP 服务：

```go
listener, err := rtcp.Listen(tunnelCtx, rtcp.Config{
    Server: "relay.example.com:1080",
    Bind:   "0.0.0.0:0", // 请求公网端分配空闲端口，也可指定固定端口
})
if err != nil { return err }
defer listener.Close()
log.Printf("远端监听地址：%s", listener.Addr())
server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
return server.Serve(listener)
```

上面是入口片段，`handler`、`tunnelCtx` 由宿主提供。`Listen` 成功时远端已绑定，`Accept()` 返回的连接已消费 Relay 协议头，`RemoteAddr()` 是访客地址。全程不拨号本地目标，不调用本地 `net.Listen`。无需创建 `Client` 或提供 `target`。

关闭时先调用 `server.Shutdown` 让请求完成，再取消 `tunnelCtx`；直接取消它会立即断开所有连接。完整的启动、停止和错误处理见 [Gin 示例](src/examples/gin/main.go) 与 [JmptJwxtAPI 接入说明](docs/business-entry.md)。

运行独立 Gin 示例（依赖只属于示例 module，版本与参考项目一致）：

```powershell
$env:GOST_SERVER = 'relay.example.com:1080'
$env:GOST_BIND = '0.0.0.0:0'
# 服务端要求认证时设置 GOST_USER、GOST_PASSWORD。
cd src/examples/gin
go run .
```

从日志取得实际端口后，访问 `http://公网IP:实际端口/ping`，也可 POST JSON 到 `/echo`。重连会再次请求绑定；端口 `0` 可能得到不同端口，通过 `listener.Addr()` 或绑定日志读取最新值。

## 转发已有 TCP 服务

目标转发程序示例：

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
    client, err := rtcp.NewClient(cfg, "127.0.0.1:80")
    if err != nil { log.Fatal(err) }
    // 配置变更回调中可调用 client.UpdateTarget("127.0.0.1:8081")。
    if err := client.Run(ctx);
        err != nil && !errors.Is(err, context.Canceled) {
        log.Fatal(err)
    }
}
```

运行期间可从配置变更回调调用：

```go
if err := client.UpdateTarget("127.0.0.1:8081"); err != nil {
    log.Printf("更新失败，保留原目标: %v", err)
}
```

更新只影响尚未选定目标的新流，已有连接继续使用原目标，不重建 TLS、smux 或远端监听。可并发调用；目标需为非空 host 和 1..65535 数字端口，不进行 DNS 查询或连通性探测。远端协议目标覆盖仍优先。旧包级 `Run` 已移除。

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

仅单元检查可使用 `-UnitOnly`，它不代表官方互通检查通过。完整检查会验证官方二进制版本，并拒绝测试 SKIP；同时对 SDK 和独立 Gin 示例执行 tidy、build、vet、race 测试。根目录的 `go test ./...` 不包含嵌套 Gin module，请用脚本执行完整检查。官方测试只访问本机回环地址。CI 定义见 [.github/workflows/checks.yml](.github/workflows/checks.yml)；尚未在远程平台运行。

## 工程与版本管理

源码与测试在 `src/`，示例在 `src/examples/`，参数文档在 `docs/`。不需要上层 demo、report 或旧 rtcp 目录才能构建。

`main` 是唯一长期开发主线；短期分支、独立 PR、审查与检查、Squash 合入，以及不可变版本 Tag 的规则见 [AGENTS.md](AGENTS.md)。当前没有配置远程仓库，不声称已启用平台分支保护或完成远程 PR。首次建库提交只建立已经验证的 SDK 工程基线，尚未创建正式发布 Tag。

## 开源声明

本项目使用 [MIT License](LICENSE)。核心代码精简自 [go-gost/x v0.16.0](https://github.com/go-gost/x/tree/14fed91c6245289e2e92ea60a97332a62c90fad0)，保留原作者版权，来源映射见 [NOTICE](NOTICE)。

| 依赖 | 版本 | 用途 | 许可 |
|---|---|---|---|
| [go-gost/relay](https://github.com/go-gost/relay) | v0.7.0 | Relay 请求、响应及 Feature 编解码 | [MIT](THIRD_PARTY_LICENSES/relay.txt) |
| [xtaci/smux](https://github.com/xtaci/smux) | v1.5.31 | TCP 上的多路复用、保活与流控 | [MIT](THIRD_PARTY_LICENSES/smux.txt) |

业务入口复用上述依赖与 Go 标准库，没有新增 SDK 运行依赖。[独立 Gin 示例](src/examples/gin/go.mod) 使用 Gin v1.11.0（[MIT](https://github.com/gin-gonic/gin/blob/v1.11.0/LICENSE)），其传递依赖由示例的 `go.mod` / `go.sum` 单独记录，不进入 SDK 的依赖图。示例仅用于演示 HTTP 接入，未复制 JmptJwxtAPI 的业务实现。

官方 GOST 程序仅用作互通测试对端，不作为 SDK 运行依赖打包。本工程与上述项目无官方隶属关系。
