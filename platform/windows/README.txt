Modbus AI Studio {{VERSION}}（Windows x64 绿色版）

双击 ModbusAIStudio.exe 运行，不用安装。
请看中文是否为微软雅黑 UI、150% 缩放下窗口是否完整、
标题栏和任务栏的小图标是否清晰。首次启动如果仍慢，请发送
%AppData%\ModbusAIStudio\app.log（里面记录了各启动阶段耗时）。
- 系统要求：Windows 10 / 11 64 位。不支持 Windows 7 / 8。
- 需要显卡支持 OpenGL 2.1，不支持时程序会弹窗说明。处理办法：下载 Mesa3D 软件渲染版的
  opengl32.dll（https://github.com/pal1000/mesa-dist-win 的 x64 目录），放到 exe 同一目录再运行。
- 报文记录保存在 %AppData%\ModbusAIStudio\packets.db，保留 7 天。
- modbus-sim.exe 是命令行模拟从站（换热站示例点表），modbus-cli.exe 是命令行主站，
  在命令行里加 -h 查看用法。

使用说明：https://github.com/xiaolengWangWang/modbus-ai-studio/blob/main/docs/guide.md
常见问题：https://github.com/xiaolengWangWang/modbus-ai-studio/blob/main/docs/faq.md
项目主页：https://github.com/xiaolengWangWang/modbus-ai-studio
