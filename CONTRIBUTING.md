# 贡献指南

## 开发环境

- Go 1.27+,无 CGO,依赖已 `go mod vendor`(`go.bug.st/serial` 与 `golang.org/x/sys`)
- **三平台支持(Linux/macOS/Windows)**:平台差异全部收敛在 build-tag 分文件里
  (`internal/device/enumerate_{linux,windows,darwin,other}.go`、
  `portholders*.go`、`internal/ctl/socketpath_*.go`),编辑时保持守卫
- 改了 `go.mod` 之后执行 `go mod vendor` 并把 vendor 目录一并提交(离线可构建是特性)

## 常用命令

```bash
make build        # go build -o serialtap .
make test         # go test -race -count=1 ./...
make cover        # 覆盖率(排除 internal/testutil)
make lint         # golangci-lint(配置见 .golangci.yml)
make fmt          # gofmt
make vet          # go vet
go test -race ./internal/collector/   # 聚焦单个包
```

CI 门禁(`.github/workflows/ci.yml`):golangci-lint、ubuntu 测试 + **80% 覆盖率下限**
(coverpkg 排除 `internal/testutil`)、**三平台测试矩阵**(ubuntu/windows/macos 各跑全量
`go test`)、linux/darwin/windows × amd64/arm64 交叉构建。
提 PR 前本地先过 `make lint test`。

## 代码结构

`main.go` 是薄入口,全部实现按职责分包在 `internal/` 下,依赖单向、无环。
**改代码前先读 [docs/architecture.md](docs/architecture.md)** —— 那里有包依赖图、
采集器状态机、release/flash 编排与设计纪律的完整描述。速查:

| 包 | 职责 |
|---|---|
| `cli` | 子命令分发与编排 |
| `config` | 配置定义与加载 |
| `device` | 设备发现与稳定身份(by-path key) |
| `collector` | 单设备采集器(open-once-and-hold) |
| `logstore` | 双通道日志 / 轮转 / 保留期清理 |
| `signature` | 错误签名引擎 |
| `pause` | PAUSED 暂停清单 |
| `daemon` | 热插拔守护循环 + release/flash 编排 |
| `flash` | esptool 编排 + flasher_args.json 解析 |
| `ctl` | 控制 unix socket(JSON 行协议) |
| `analyze` | 离线分析(签名汇总 + addr2line) |
| `testutil` | 跨包测试助手,**只被测试导入** |

## 测试方式(TDD)

项目按 TDD 开发,核心思路是**注入 seam 让全链路离线可测**(seam 一览见
[docs/architecture.md](docs/architecture.md#可测性设计seam-一览)):

- 采集器全链路:`collector.OpenPort` 是包级变量,测试替换为 `testutil.FakePort`
  (预置数据块按序吐出)
- 守护循环:`daemon.New(enum, logf)` 注入假枚举
- 设备发现:`device.buildDevices` 是纯函数,端口清单/by-id/by-path/VID:PID 全注入;
  Windows 注册表扫描用 HKCU 假 Enum 树测(`usbSerialMetaAt` 根键可注入)
- 假外部命令(esptool/addr2line 替身):用 `testutil.FakeTool()`
  (编译成真二进制 —— shell 脚本假件在 Windows 不可执行;行为由 FAKE_EXIT/FAKE_OUT
  环境变量驱动,`t.Setenv` 配置即可)
- fuzz:行拼装器与 Backtrace 地址提取各有一个 `_fuzz_test.go`
- 端到端:socat PTY 模拟真串口(Linux)

写新功能时同样优先设计 seam,而不是 mock 到处穿;错误分支(EBUSY、坏正则、
非预期 JSON 值)都要有测试。

## 平台注意事项

- 平台差异代码必须有 build tag 守卫;涉及"设备身份/占用检测/socket 路径"的改动,
  三平台(`GOOS=linux/darwin/windows`)至少 `go build ./... + go vet ./...` 一遍
- 涉真串口/真 /proc 的测试是 `*_linux_test.go`;Windows 注册表测试是
  `*_windows_test.go`(HKCU fixture,无需管理员)—— 在任一台开发机上总有部分平台测试
  被跳过,靠 CI 三平台矩阵兜底
- Windows 上文件被打开时不可删除:测试里起了采集协程的,务必 `t.Cleanup(d.Shutdown)`
  收尾,否则 `t.TempDir()` 清理会因文件占用而失败

## 规范

- **注释与文档用简体中文**,提交信息用英文 conventional commits 风格:
  `feat: ...`、`fix(scope): ...`、`docs: ...`、`refactor: ...`
- 注释写给"下一个改这段代码的人":写约束和为什么(如 collector 文件头的四条设计
  要点),不复述代码做了什么
- `fmt.Print*` 族写失败不查(errcheck 已豁免);其余错误都要处理
- 用户可见文档:README(用户入口)、`docs/`(深入主题)、`CHANGELOG.md`(每版本)

## 敏感区改动前必读

| 要动的区域 | 先读 |
|---|---|
| 采集器生命周期、重开/看门狗逻辑 | README"复位语义"节 + `internal/collector/collector.go` 文件头注释 |
| 刷写/让口编排 | [docs/ctl-protocol.md](docs/ctl-protocol.md) + `internal/daemon/flash.go` |
| 设备身份/命名 | [docs/architecture.md](docs/architecture.md#设备身份模型) |
| 日志格式(下游有 analyze 依赖时间戳格式) | `internal/logstore/logstore.go` 的 `tsFormat` 与 `internal/analyze` 的解析正则 |

## 发布流程

1. 更新 `CHANGELOG.md` 与 `internal/cli` 的 `Version` 常量
2. 打 tag(如 `v0.1.1`)推送 —— `release.yml` 自动构建 linux/darwin/windows 的
   amd64/arm64 产物并附到 GitHub Release
