# 业务入口变更记录

## 逻辑目标

原 SDK 只能把每条远端流拨号转发到 TCP target，嵌入式业务必须另外启动本地监听端口。新增 `Listen(ctx, Config) (net.Listener, error)`，让其他 Go 项目直接消费连接，HTTP/Gin 使用标准 http.Server.Serve，SDK 不绑定框架。

新入口复用原 TLS、BIND、Relay 解析和 smux，返回实际绑定地址及访客地址，支持运行期重连；初次失败同步返回。Listener.Close 允许已接收连接排空，context 取消强制回收。目标覆盖头只消费、不执行，因为该入口没有 TCP target。

原 Client.Run / UpdateTarget 仍负责已有 TCP 目标转发，没有新增兼容层。未修改 pipe、原版半关闭限制或空主机绑定缺陷。SDK 依赖版本不变；Gin v1.11.0 只属于独立示例 module。README、配置、行为、接入文档和 NOTICE 同步更新。

## 验证

完整检查命令：

```powershell
pwsh -File scripts/check.ps1 -GostBinary .tools/gost/gost.exe
```

检查范围：gofmt；两个 module 的 tidy 无变化、build、vet、race 测试；SDK 外部 Go module 引用；官方 GOST v3.3.0 互通，完整模式拒绝 SKIP。结果记录在 `.tools/checks.jsonl`、`.tools/gin-checks.jsonl`（本地生成文件，不提交）。

2026-10-08 在 Windows / Go 1.23.3 实际执行通过：SDK 日志 37 条测试/示例通过结果（含子测试），Gin module 1 条通过结果，失败和测试跳过均为 0；脚本最终输出 `PASS: complete SDK checks, no skipped tests`。官方二进制版本为 v3.3.0，SHA-256 为 `6f6b66cbdc4a5aa5685d0017abd3f65843a28bcc2bb4e5b78d6d12a844a51aa8`，与仓库 CI 固定值一致。文档链接和全部新增/修改文件的空白检查也通过；未执行远程 CI 或声称他人批准。

重点覆盖：没有本地业务监听的并发字节转发、Relay 头消费及目标覆盖忽略、访客地址与地址快照、单连接关闭与期限隔离、Listener.Close 与 context 取消、端口释放、重连后的新旧连接、HTTP keep-alive 与 Shutdown、Gin GET/POST/ClientIP 和退出排空。原有目标转发测试继续运行。

JmptJwxtAPI 仅作只读结构参考，未修改、未运行真实教务及 OCR 业务。

## 行为变化与审查

这是新增业务入口的独立逻辑目标。工作分支 `feat/business-listener` 从本地 main 建立后，快进承接尚未合入的 `refactor/src-layout`、`feat/target-hot-update` 前置提交；本次差异以 `8e80107` 为基线。未合并 main，未创建 Tag 或发布。

当前没有远程 PR 平台。代码与检查结果供审查，代理自检不代表他人批准。接入平台后需先完成前置改动的独立审查，再为本次目标创建独立 PR，审查和必需检查通过后按仓库规则 Squash 合入。
