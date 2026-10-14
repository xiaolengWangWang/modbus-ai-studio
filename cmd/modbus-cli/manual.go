package main

import (
	"flag"
	"strings"
)

const usage = `用法：modbus-cli [选项] <命令> [参数]（选项写在命令前面）
  read <地址> <数量> · write <地址> <值> · detect · scan [起始 结束] [地址] · raw <PDU>
  probe [auto|single|quick] <地址> <数量> · -points 点表 probe points|each
  -points 点表 points · get [点名…] · set <点名> <值> · manual · version
运行 modbus-cli manual 查看完整说明。

选项：
`

// manualText 是完整说明，modbus-cli manual 打印它，打包时也存成包里的 README.txt。
const manualText = `Modbus AI Studio 命令行工具 modbus-cli {{version}}

modbus-cli 是命令行的 Modbus 主站，和桌面版用同一套通信核心：读写寄存器和线圈（写入后自动回读核对）、
识别协议、搜索站号、检测哪些寄存器读得通、发自定义请求。加 -json 后每条命令在标准输出打印一个 JSON 对象，脚本和 AI 可以直接解析。
不需要安装，解压后在终端里运行（Windows 是 modbus-cli.exe）；macOS 提示无法验证开发者时，运行 xattr -d com.apple.quarantine modbus-cli。

用法
  modbus-cli [选项] <命令> [参数]
  选项必须写在命令前面：modbus-cli -json read 40001 4 可以，modbus-cli read 40001 4 -json 不行。

命令
  read  <地址> <数量>    读数据。寄存器按 -type / -order / -scale 解成工程值，线圈和离散输入读出 0 / 1
  write <地址> <值>      写入并立即回读核对。寄存器按 -type / -order / -scale 编码，线圈写 0 或 1
  detect                识别 Modbus TCP 还是 RTU over TCP（需要 -target）
  scan [起始 结束] [地址]
                        搜索应答的站号，默认 1–247：每个站号依次读 <地址>（可不写）、40001、30001、线圈 1，
                        有一次应答就算找到（异常响应也算；网关回的异常 0A、0B 不算）。没指定 -timeout 时每次读取等 200 ms。
                        设备上肯定有的地址写成 <地址>，不回非法地址的设备也能搜到。按 Ctrl+C 停止，保留已找到的站号
  raw <PDU 十六进制>     发任意请求 PDU（功能码 + 数据），打印响应 PDU，例如 raw 08 00 00 12 34
  probe [方式] <地址> <数量>
                        检测寄存器：找出这段地址里哪些读得通、哪些是非法地址（异常 02）、哪些不响应，
                        和桌面版“检测寄存器”同一套算法。方式（不写就是 auto）：
                          auto    自动定位：批量读，读不通的段再逐个定位到单个地址（最快）
                          single  逐个寄存器：每个地址单独读一次，找出全部读不通的地址
                          quick   快速排查：批量读，从返回全 0 的段里抽查单独读，看设备是不是给没数据的地址补 0；
                                  最多 16 次验证请求，有 -points 时按点读
  probe points | each   按 -points 点表检测（-fc 选数据区，不写时依次找 03、04、01、02 里有点的）：
                          points  按点表分段：地址相连的点一段读一次，读不通的段逐点再读
                          each    逐点独立验证：每段整段读一次，再每个点单独读一次，找出整段有返回、单独却读不通的点
                        结果说明里的“逐个寄存器”“逐点独立验证”就是 single、each。
                        进度和预计剩余时间每 2 秒打印到标准错误；按 Ctrl+C 停止，保留已完成的结果。
                        -report 报告.md 另外写出检测报告（Markdown 摘要、同名 JSONL 请求明细、CSV 失败清单）
  points                列出 -points 点表里的点（名称、地址、类型、单位、可写、范围、枚举），不连设备
  get [点名…]           按点名读工程值，带单位和枚举含义；不写点名时读全部点
  set <点名> <值>        按点表的类型、字节序、倍率写一个点，检查可写和允许范围，枚举点可写含义（例如 自动）；
                        写入后在 0、200、1000 ms 回读，判断通过、未生效、被改回还是值不符
  manual                打印本说明
  version               打印版本号

点表（-points）
  和桌面版“导入点表”认同样的文件：本程序的点表（地址,名称,类型,字节序,倍率,单位,读写,最小,最大,枚举,长度）
  或物联网平台导出的设备属性表（属性标识、属性名称……），CSV 或 xlsx。点名写全名，或写能唯一对上的几个字；
  点名重复时写地址。相近的点合成一次读取，某一段读不出来只影响那一段的点。

地址写法
  40001、400001、4x0001    保持寄存器：功能码 03 读，06 / 16 写
  30001、300001、3x0001    输入寄存器：04 读
  10001、100001、1x0001    离散输入：02 读
  346、0x015A              原始协议地址（Offset，从 0 开始），默认按保持寄存器；
                           线圈用 -fc 指定：-fc 1 read 0 8 读线圈，-fc 5 write 0 1 写线圈
  40001 这类 5 位地址也可能是原始 Offset 40001，按 4x 执行并在标准错误提示；要原始 Offset 就写成十六进制 0x9C41。

连接
  网络设备  -target 192.168.1.100:502，默认 Modbus TCP；串口服务器 / DTU 透传用 -mode rtu-over-tcp
  串口设备  -port COM3 -baud 9600 -parity N -stop 1，默认 Modbus RTU；ASCII 用 -mode ascii -databits 7
  站号用 -slave，超时用 -timeout（例如 3s）。读请求超时或校验错误时自动重试 1 次，写请求不重试。

选项
{{options}}
JSON 输出（-json）
  成功时 "ok": true，各命令的字段：
    read     mode、slave、function、start、count、type、order、elapsed_ms、values
             values 每项：address、offset、registers（十六进制）、value（工程值，字符串，64 位整数不丢精度）、
             hint（数值不合理时建议的字节序）
    write    address、function、value（实际写入的值）、rounded、registers、request（请求帧）、readback、verified
    detect   mode、slave、by_exception、attempts（每次尝试的 mode、slave、result）
    scan     from、to、probes（每个站号依次试的读取）、stopped（按了 Ctrl+C）、found（每项 slave、probe、rtt_ms、note）
    raw      slave、request、response（都是 PDU 的十六进制）
    points   file、format、count、points（每项 name、address、type、order、scale、unit、writable、min、max、enum）、skipped
    get      mode、slave、elapsed_ms、values（每项 name、address、value、unit、registers、hint、error）
    set      name、address、original、target、unit、result（PASS 等）、hint、readbacks（每项 at_ms、value、error）
    probe    mode、slave、function、start、offset、count、points（按点表时的点数）、requests、elapsed_ms、
             summary（一行统计）、problems（有读不通或无法判断的地址）、readable（最长的连续可读范围：start、offset、count，
             常规读取可以改用，auto 和 single 才有）、failures（读不通的地址或点，字段和 CSV 失败清单一样）、
             text（完整说明，和桌面版结果框一样）、report（-report 写出的文件）
    version  version
  失败时 "ok": false，error 是原因和建议；write 回读不符、raw 收到异常响应时也带上收到的内容。
  probe 检测完就是 ok（查出读不通的地址也算完成，看 problems 和 failures）；中途停止时 ok 为 false，
  已完成的结果照样给出。
  -trace 的收发报文打印到标准错误，不影响标准输出的 JSON。

退出码
  0 成功；1 连接或通信失败、异常响应、写入后回读不符、检测中途停止；2 命令或参数写错。

示例
  modbus-cli -target 192.168.1.100:502 -type float32 -order CDAB read 40001 4
  modbus-cli -target 192.168.1.100:502 -json read 30001 10
  modbus-cli -target 192.168.1.100:502 -fc 1 read 0 16
  modbus-cli -target 192.168.1.100:502 -type float32 -order CDAB write 40347 16
  modbus-cli -target 192.168.1.100:502 -fc 5 write 0 1
  modbus-cli -target 192.168.1.100:502 detect
  modbus-cli -port COM3 -baud 9600 scan 1 32
  modbus-cli -target 192.168.1.100:502 scan 1 247 40101
  modbus-cli -target 192.168.1.100:502 -slave 3 raw 08 00 00 12 34
  modbus-cli -target 192.168.1.100:502 probe 40001 200
  modbus-cli -target 192.168.1.100:502 -timeout 3s -json probe single 40001 50
  modbus-cli -target 192.168.1.100:502 -points 点表.xlsx -report 检测报告.md probe each
  modbus-cli -points 点表.xlsx points
  modbus-cli -target 192.168.1.100:502 -points 点表.xlsx -json get 二次供温 二次回温
  modbus-cli -target 192.168.1.100:502 -points 点表.xlsx set 温差设定 16
  modbus-cli -port /dev/ttyUSB0 -baud 9600 -trace read 40001 10

给 AI 调用
  1. 先运行 modbus-cli manual 了解命令和参数。
  2. 每次调用都加 -json，按 ok 判断成败，失败时把 error 告诉用户。有点表时优先用 points 看有哪些点，再用 get / set 按点名读写。
  3. write、set 会改变现场设备的状态（启停、设定值），调用前先向用户确认点（地址）和值。
  4. 读不出来时先 detect（网络设备）或 scan（找站号），再加 -trace 看收发报文。
  5. 读一段报异常 02（非法数据地址）时，用 probe 找出读得通的范围，按 readable 改读取范围；
     有点表时用 probe each 找出读不通的点。检测会发很多请求，范围大时先告诉用户大概要多久。
`

// manual 返回完整说明，选项部分取自 flag 的定义，和 -h 一致。
func manual() string {
	var opts strings.Builder
	flag.CommandLine.SetOutput(&opts)
	flag.PrintDefaults()
	flag.CommandLine.SetOutput(nil)
	return strings.NewReplacer("{{version}}", version, "{{options}}", opts.String()).Replace(manualText)
}
