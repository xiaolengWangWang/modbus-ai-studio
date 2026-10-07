Modbus AI Studio {{VERSION}}（Windows x64 绿色版）

双击 ModbusAIStudio.exe 运行，不用安装。
可同时运行多个程序实例，也可用“文件 → 新建窗口”打开独立工作区。
右上角关闭按钮默认隐藏到托盘，采集继续；右键托盘图标选择“退出所有窗口”退出本实例。
“视图 → 关闭按钮隐藏到托盘”可修改关闭行为；窗口大小自动适配屏幕并记住上次设置。
“调试 → 检测寄存器”逐个检测指定地址范围，区分可读、非法地址（异常 02）、无法判断和未检测；可停止、复制结果。
请看中文是否为微软雅黑 UI、150% 缩放下窗口是否完整、
标题栏和任务栏的小图标是否清晰。首次启动如果仍慢，请发送
%AppData%\ModbusAIStudio\app.log（里面记录了各启动阶段耗时）。
- 系统要求：Windows 10 / 11 64 位。不支持 Windows 7 / 8。
- 需要显卡支持 OpenGL 2.1，不支持时程序会弹窗说明。处理办法：下载 Mesa3D 软件渲染版的
  opengl32.dll（https://github.com/pal1000/mesa-dist-win 的 x64 目录），放到 exe 同一目录再运行。
- 报文记录从 %AppData%\ModbusAIStudio\packets.db 开始，单个数据库文件上限 50 MB。
  写满后自动创建 packets-000001.db 等编号文件，旧文件保留，不自动删除。
  “历史记录”可选择文件查看；所有 .db 文件也可用 SQLite 管理工具打开。
- modbus-sim.exe 是命令行模拟从站（换热站示例点表），modbus-cli.exe 是命令行主站，
  在命令行里加 -h 查看用法。

使用说明：https://github.com/xiaolengWangWang/modbus-ai-studio/blob/main/docs/guide.md
常见问题：https://github.com/xiaolengWangWang/modbus-ai-studio/blob/main/docs/faq.md
项目主页：https://github.com/xiaolengWangWang/modbus-ai-studio
