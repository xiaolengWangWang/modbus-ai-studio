# SQLite3 数据文件与表结构

程序直接读写标准 SQLite3 文件，SQLite 引擎随 Go 程序使用，不构建或附带 `sqlite3.exe` 等命令行工具。桌面版现有文件名为 `packets.db`、`packets-000001.db`；按天记录时为 `packets-YYYYMMDD-001.sqlite3`。扩展名不同，文件格式相同。

建表定义统一保存在 `internal/recorder/recorder.go` 的 `schema` 中。下面说明字段含义和查询方式，不维护另一份建表脚本。

## 表与关联

| 表 | 用途 | 主键与关联 |
| --- | --- | --- |
| `sessions` | 一次设备连接的起止、协议、目标和窗口 | `id` 为整数主键；会话编号跨进程随机起点递增，不能作为时间使用 |
| `packets` | 每次发送、接收和失败结果 | `id` 为文件内整数主键；`session_id` 引用 `sessions.id`，删除会话时级联删除 |
| `events` | 连接、故障、恢复及诊断日志 | `id` 为文件内整数主键；`session_id` 同样引用会话 |

跨文件继续写入的会话会复制其元数据到新文件。`packets.id`、`events.id` 仅在各自文件内唯一；跨文件定位记录时需要同时保存文件路径。外键在写入连接上启用。

### sessions

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `id` | INTEGER PRIMARY KEY | 会话编号 |
| `started_at` | INTEGER NOT NULL | 开始时间，Unix 毫秒 |
| `ended_at` | INTEGER，可为 NULL | 结束时间，Unix 毫秒；NULL 表示未正常结束，也可能仍在连接 |
| `protocol` | TEXT NOT NULL | `MODBUS_TCP`、`RTU_OVER_TCP`、`ASCII_OVER_TCP`、`MODBUS_RTU`、`MODBUS_ASCII` |
| `target` | TEXT NOT NULL | IP 与端口，或串口及通信参数 |
| `window` | INTEGER NOT NULL | 主窗口编号 |

### packets

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `id` | INTEGER PRIMARY KEY | 文件内报文编号，排序和游标使用此字段 |
| `session_id` | INTEGER NOT NULL | 所属会话 |
| `time` | INTEGER NOT NULL | 记录时间，Unix 微秒 |
| `request_id` | INTEGER NOT NULL | 同一请求的 TX 与结果行共用的请求编号 |
| `connection_id` | TEXT NOT NULL，默认空串 | 连接代次；旧记录为空 |
| `direction` | TEXT NOT NULL | `TX` 或 `RX` |
| `protocol` | TEXT NOT NULL | 协议名称，与会话协议取值一致 |
| `slave`、`tx_id` | INTEGER NOT NULL | 站号、Modbus TCP Transaction ID |
| `function` | INTEGER NOT NULL | 功能码数值，例如 3 表示 FC03 |
| `address`、`count` | INTEGER NOT NULL | 请求起始地址（协议零基偏移）、数量 |
| `raw` | BLOB，可为 NULL | 原始报文字节，或其 zlib 压缩结果；超时等无报文时为 NULL |
| `raw_codec` | INTEGER NOT NULL，默认 0 | 0 原文，1 zlib |
| `status` | TEXT NOT NULL | `SENT`、`SUCCESS`、`TIMEOUT`、`EXCEPTION`、`CRC_ERROR`、`CONNECTION_ERROR`、`PARSE_ERROR`、`CANCELLED`、`LATE_RESPONSE`、`UNEXPECTED` |
| `rtt_us` | INTEGER NOT NULL | 响应耗时，微秒；发送行通常为 0 |
| `error` | TEXT，可为 NULL | 错误文字 |

### events

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `id` | INTEGER PRIMARY KEY | 文件内事件编号 |
| `session_id` | INTEGER NOT NULL | 所属会话 |
| `time` | INTEGER NOT NULL | 日志时间，Unix 毫秒 |
| `kind` | TEXT NOT NULL | `CONNECT`、`CONNECT_FAIL`、`READ_FAIL`、`READ_OK`、`DISCONNECT`、`RECONNECT` |
| `detail` | TEXT NOT NULL；压缩时存为 BLOB | 事件结论 |
| `window` | INTEGER NOT NULL，默认 0 | 读取窗口编号；连接事件为 0 |
| `analysis` | TEXT NOT NULL，默认空串；压缩时存为 BLOB | 原因分析和报文字段解析 |
| `tx`、`rx` | BLOB，可为 NULL | 原始请求、响应，或各自的 zlib 压缩结果 |
| `data_codec` | INTEGER NOT NULL，默认 0 | 按位标记压缩字段：1 TX、2 RX、4 analysis、8 detail；各位可组合 |

## 压缩与兼容

大于等于 256 字节的报文、日志文本使用 zlib BestSpeed 尝试无损压缩，只在结果更小时保存压缩数据。短报文及不可压缩内容保持原样；协议、时间、会话编号、状态等查询字段不压缩。压缩和解压的工作内存复用，通信回调只复制报文并入队，压缩在后台批量写盘时完成。

普通日志文本仍存为 TEXT。SQLite 的普通表允许 TEXT 亲和列保存 BLOB，因此压缩后的 `detail`、`analysis` 存在原列中，由 `data_codec` 区分。[SQLite 类型说明](https://sqlite.org/datatype3.html)

应用查询时自动还原。数据库管理工具可以直接查看表结构、编号、时间、状态及短文本；读取压缩字段需要按 codec 解码。例如 Python 自带的 sqlite3 与 zlib 模块即可读取，不需要 SQLite 命令行程序：

```python
import sqlite3
import zlib

with sqlite3.connect("file:packets.sqlite3?mode=ro", uri=True) as db:
    for raw, codec in db.execute(
        "SELECT raw, raw_codec FROM packets WHERE session_id = ? ORDER BY id DESC LIMIT 100",
        (session_id,),
    ):
        raw = zlib.decompress(raw) if codec == 1 else raw

    for analysis, codec in db.execute(
        "SELECT analysis, data_codec FROM events WHERE session_id = ? ORDER BY id",
        (session_id,),
    ):
        if codec & 4:
            analysis = zlib.decompress(analysis).decode("utf-8")
```

旧文件可直接读取；作为当前写入文件打开时自动补 codec 列，已有记录默认为原文，不修改原始内容。只读历史查询不会迁移旧文件。新压缩记录需使用支持 codec 的版本读取；程序不会通过猜测字节头来解码旧报文。损坏或未知编码明确报错，压缩数据解压后最多 64 MiB，超过此大小的原文不压缩。

## 大数据量查询

| 索引 | 查询用途 |
| --- | --- |
| `sessions_recent(started_at DESC, id DESC)` | 最近会话，时间相同时以编号排序 |
| `packets_session(session_id, id)` | 按会话取最后若干条报文、游标分页、会话计数 |
| `packets_time(time)` | 按时间定位和清理报文 |
| `events_session(session_id, id)` | 会话日志、故障与断开计数 |

历史会话列表先按索引取最近 N 个会话，再统计这些会话的报文与事件。统计只读状态等元数据，不读取或解压 BLOB。报文列表使用 `ORDER BY id DESC LIMIT ?`，只解压返回行。

外部工具分页可用编号游标，避免大量 OFFSET 跳过行：

```sql
SELECT id, time, direction, function, address, count, status
FROM packets
WHERE session_id = ? AND id < ?
ORDER BY id DESC
LIMIT 100;
```

上述查询定位单个文件；主文件上限为 50,000,000 字节，按文件实际页大小向下取整设置页数限制，索引也计入上限。初始化和旧库升级前就设置限制，写满或升级空间不足时自动换文件，旧文件保留。已有超过上限的旧文件不删除或缩减，新写入使用续建文件。查询成本取决于选中会话的实际数据量，读取全部日志仍需要遍历该会话全部事件。可用 `EXPLAIN QUERY PLAN` 检查索引使用，输出格式仅用于诊断。[SQLite 查询计划说明](https://sqlite.org/eqp.html)

本机 pure Go 驱动基准（相同数据、每项 3 次）：2000 个会话、20 万条报文、2 万条事件，查询最近 20 个会话的统计从约 5.550 秒降到 3.171 毫秒；500 条同会话报文批量写入从 9.710 毫秒降到 6.201 毫秒。这些数值用于比较本次改动，实际耗时随设备、驱动和数据分布变化。

## 写入与文件可靠性

后台每 200 ms 或 500 条报文写一次事务；同一批中的同一会话只检查一次。队列满或写入失败计入 Dropped，关闭时等待全部缓冲处理完毕并返回后台写入错误。多个调用方同时关闭也等待同一次关闭完成。同步日志、创建会话和结束会话在关闭后返回 ErrClosed，跨天也不会重新打开数据库。

旧库升级在 BEGIN IMMEDIATE 写锁内检查并新增列，失败时整次回滚。两种驱动共用经过转义的文件 URI，特殊字符路径不会读到其他文件。

WAL 支持查询与写入并行；运行中的数据库可能还有 `-wal`、`-shm` 文件。导出使用 `recorder.Snapshot`（VACUUM INTO），生成包含已提交 WAL 数据的独立 SQLite3 文件，不覆盖已有目标。仍在异步队列中的记录需等写盘或正常关闭后才进入快照。[SQLite WAL 说明](https://sqlite.org/wal.html)

## 验证命令

```sh
go test -count=1 ./internal/recorder
go test -run '^$' -bench 'BenchmarkSessionsLargeArchive|BenchmarkInsertBatchSameSession|BenchmarkEncodeData|BenchmarkDecodeData' -benchmem -benchtime=3x ./internal/recorder
```

CI 保留桌面 cgo 驱动测试，同时单独运行 `CGO_ENABLED=0` 的纯 Go 驱动回归。两种驱动都写标准 SQLite3 文件。
