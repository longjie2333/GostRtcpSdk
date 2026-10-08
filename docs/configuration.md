# 配置与 API

包名是 `rtcp`，module 标识为 `example.com/gostrtcpsdk`，导入路径为 `example.com/gostrtcpsdk/src`。

```go
func Listen(ctx context.Context, cfg Config) (net.Listener, error)
func NewClient(cfg Config, target string) (*Client, error)
func (c *Client) Run(ctx context.Context) error
func (c *Client) UpdateTarget(target string) error
```

NewClient 不建立网络连接。Client.Run 会阻塞，同一实例只允许一个 Run 同时执行，重复调用返回错误。取消并等 Run 返回后可再次运行，保留最新目标。必须通过 NewClient 构造，实例不可复制；没有额外 Start/Stop 或全局单例。

| 参数/字段 | 类型 | 要求与默认行为 |
|---|---|---|
| `ctx` | `context.Context` | 非 nil，由宿主管理；取消时强制结束隧道及连接。Run 返回取消错误；Listen 的 Accept 返回 net.ErrClosed |
| `cfg.Server` | `string` | 官方 Relay+TLS 服务地址 `host:port`，不带 URL scheme；格式错误直接返回 |
| `cfg.Bind` | `string` | 远端监听地址，显式填写 IP 和端口，如 `0.0.0.0:8080`；不能依赖空主机形式 |
| `cfg.User` | `*url.Userinfo` | 使用 `url.UserPassword` 设置认证；nil 不发送认证 Feature，对端可能拒绝 |
| `cfg.TLS` | `*tls.Config` | nil 时默认不验证证书；非 nil 会 Clone；未设置 ServerName 时从 Server 取主机名 |
| `cfg.Logger` | `*slog.Logger` | nil 时使用 `slog.Default()`；Info 输出实际绑定地址，Run 的 Debug 输出访客与转发事件 |
| `target` | `string` | 从 SDK 所在机器拨号的 TCP 目标，如 `127.0.0.1:80`；host 必须非空，端口为 1..65535 的数字，构造与更新使用同一校验 |

Config 及引用对象在入口使用期间保持不变。仅 Client 的 target 可通过 UpdateTarget 热更新；修改服务器、认证或绑定配置需关闭旧入口并重新创建。多个独立业务使用不同 Listener 或 Client、上下文和远端端口。

## Listen：直接交付业务连接

`Listen(ctx, cfg)` 不需要 target，成功返回标准 `net.Listener` 时，首次 TLS、认证和远端绑定已完成。首次失败直接返回错误，不在函数内无限重试；TLS 连接建立有 15 秒超时，BIND 读取等待受 ctx 控制。ctx 不仅用于启动，还管理返回 Listener 的整个生命周期，不要在 Listen 返回后立即取消。

| 操作 | 行为 |
|---|---|
| `listener.Accept()` | 阻塞等待业务连接；允许并发调用。SDK 先消费每条流的 Relay 头；不交付协议字节，不使用可选目标覆盖去拨号 |
| `listener.Addr()` | 返回最近一次成功绑定的远端 TCP 地址快照；`0.0.0.0:0` 或 `127.0.0.1:0` 请求自动分配。断线期间仍是上次地址，不是健康状态指示 |
| `conn.LocalAddr()` | 创建该连接时的远端绑定地址，重连不改变已有连接的地址 |
| `conn.RemoteAddr()` | Relay 返回的访客 TCP 地址，可供 HTTP 的 RemoteAddr、Gin ClientIP 使用 |
| `conn.SetDeadline/SetReadDeadline/SetWriteDeadline` | 直接使用 smux 的单流期限，不对共享 TLS 设置期限。超时用 `net.Error.Timeout()` 判断；不增加 TCP 半关闭能力 |
| `conn.Close()` | 业务负责关闭每个已接收连接；关闭及读写期限不影响同会话的其他连接 |
| `listener.Close()` | 幂等，停止交付新连接，唤醒所有阻塞 Accept（`errors.Is(err, net.ErrClosed)`）；允许已接收连接继续读写 |
| 取消 `ctx` | 关闭监听和当前隧道，强制终止所有连接；即使先调用过 listener.Close 仍有效 |

Listener 关闭后，为使 HTTP Shutdown 能排空请求，隧道及公网端口会保留到最后一个已接收连接 Close；期间不再向业务交付新连接。若业务遗漏 Close，宿主必须取消 ctx 回收资源。SDK 不管理业务 goroutine，不会等待任意业务代码退出。

运行期会话或访客头异常会关闭故障会话，并在后台按 `1、2、4、5` 秒重试绑定；Accept 在重连期间继续等待，HTTP Serve 无需重启。旧连接不恢复、不重放字节。每次成功绑定都会更新 Addr 并输出 Info 日志。自动分配端口可能改变；需稳定入口时明确指定固定远端端口。

`http.Server.Serve(listener)` 可以接受 Gin 引擎或任何 http.Handler；自定义 TCP 服务直接 Accept 并自行处理 net.Conn。SDK 无 Gin 专用方法或额外服务框架。完整代码与参考项目对应关系见 [业务接入说明](business-entry.md)。

## target 热更新

- 可在 Run 前、运行中、重连期间或停止后更新；校验失败返回错误，旧目标不变。不会做 DNS 查询或连通探测。
- 更新与读取由锁保护；并发更新以实际写入顺序为准，每条流只读取一次完整快照。
- 选择时点是解析访客回复之后、启动转发 goroutine 之前；已经选定目标的拨号和已有连接不迁移、不打断。
- 远端可选第二 AddrFeature 仍覆盖默认 target；UpdateTarget 不改变这一源协议语义。
- 暂时不可达但语法正确的目标可以更新成功，实际拨号失败时仅关闭对应流。
- 旧包级 Run 删除，使用 NewClient → Client.Run。

## 证书验证

```go
roots, err := x509.SystemCertPool()
if err != nil { return err }
cfg.TLS = &tls.Config{
    RootCAs: roots,
    ServerName: "relay.example.com",
}
```

私有 CA 可用 `x509.NewCertPool()` 和 `AppendCertsFromPEM` 加载。凭据由宿主的配置系统或环境提供，不建议写入源码或日志。SDK 只加密客户端到官方 GOST 的 TLS 链路。

## Client.Run 的运行状态和错误

- 调用 Run 不表示远端已经完成绑定；Info 的 `remote TCP listening` 表示本次绑定成功，并包含分配出的真实端口（支持显式 `127.0.0.1:0`）。
- TLS/绑定失败按 `1、2、4、5` 秒退避持续重试，并记录错误，包括认证失败、对端没有启用 bind、端口冲突。它们不会作为一次 Run 的永久错误直接返回；宿主需按业务要求通过 context 限定时间或终止。
- 会话或访客回复异常会关闭会话，1 秒后尝试重新绑定；已有 TCP 会话不会恢复，也不会重放业务字节。
- 目标 TCP 拨号默认 15 秒超时，只尝试一次；失败仅关闭对应 stream。
- TCP/TLS 建立默认 15 秒期限。BIND 回复等待由调用方 context 控制；不额外增加业务空闲超时。
- 取消后等待 Run 返回，才代表 SDK 已完成清理。用 `errors.Is(err, context.Canceled)` / `context.DeadlineExceeded` 区分调用方主动停止。
- 远端第二个可选 AddrFeature 可覆盖 target，与源版本保持一致；SDK 信任所连接的官方服务端。

## 示例环境变量

`go run ./src/examples/basic` 从 `GOST_SERVER`、`GOST_BIND`、`GOST_TARGET`、`GOST_USER`、`GOST_PASSWORD` 读取配置。前三项必填；认证按需设置。示例只负责参数读取和 context，不属于 SDK 的额外配置框架。

独立 Gin 示例在 `src/examples/gin` 内执行 `go run .`，只要求 `GOST_SERVER`、`GOST_BIND`，认证变量相同，不读取 `GOST_TARGET`。
