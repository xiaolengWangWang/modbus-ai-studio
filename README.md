# Modbus AI Studio

面向工业现场的跨平台 Modbus 调试与诊断工具。

## 进度

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 | 协议核心 + 模拟器 + 自动化测试（无 GUI） | 已完成 |
| M2 | Fyne 原型，在 VM / 远程桌面 / 工控机上验证 OpenGL 与中文显示 | 未开始 |
| M3 | 调试页 + 报文监控 + Session | 未开始 |
| M4 | 控制验证 + 诊断 + 安全层 | 未开始 |
| M5 | CSV 点表 + 打包发布 | 未开始 |

## 目录

```text
cmd/modbus-sim/        命令行模拟从站（换热站示例点表）
cmd/modbus-cli/        命令行主站：读、写并回读、协议识别
internal/modbus/       协议核心：PDU、MBAP / RTU 分帧、客户端、数据类型与字节序、地址解析
internal/detect/       协议自动识别（Modbus TCP / RTU over TCP）
internal/transport/    TCP 与串口
internal/simulator/    模拟从站与故障注入
tests/                 基于模拟器的集成测试
```

## 常用命令

```sh
go test ./...                       # 单元测试 + 集成测试
go test -race -count=3 ./...        # 并发与稳定性
go build -o bin/ ./cmd/...

# 模拟器：TCP 或 RTU over TCP，可注入延迟、丢包、CRC 错误、慢应答
./bin/modbus-sim -listen 127.0.0.1:1502 -mode rtu-over-tcp -slow 600:4:1035ms

# 主站
./bin/modbus-cli -target 127.0.0.1:1502 -mode rtu-over-tcp -type float32 -order CDAB -trace read 40001 4
./bin/modbus-cli -target 127.0.0.1:1502 -mode rtu-over-tcp -type float32 -order CDAB write 40347 16
./bin/modbus-cli -target 127.0.0.1:1502 detect
./bin/modbus-cli -port /dev/cu.usbserial-110 -baud 9600 read 40001 10
```

## 协议引擎的约定

- 所有模式都按长度分帧，超时只用于异常恢复。
- 同一连接同一时刻只有一个未完成请求；响应必须通过 Transaction ID（TCP）或 Slave / 功能码 / 字节数 / 回显（RTU）校验才被采用。
- 超时后等待保护间隔并清空接收缓冲，期间收到的字节记为 `LATE_RESPONSE`，不会错配到下一条请求。
- 读请求超时或 CRC 错误后重试（默认 1 次），写请求不重试。
- 每条收发都通过 `Observer` 输出，状态值与 SQLite `packets.status` 一致。
