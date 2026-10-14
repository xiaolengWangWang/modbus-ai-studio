// Package edition 区分 Windows 普通版和兼容版。兼容版用 -tags compat 编译（build/windows.sh）：
// 用 go-legacy-win7 工具链编译，能在 Windows 7/8/8.1 上运行；exe 旁边带 Mesa 软件渲染，不依赖显卡；
// 数据和日志放在程序目录的 data 文件夹里。检查更新时兼容版只取兼容版的安装包。
package edition
