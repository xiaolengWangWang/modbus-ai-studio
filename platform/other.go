//go:build !windows

package platform

// Setup 在 macOS 和 Linux 上不需要做什么：有控制台或系统日志，OpenGL 也都有。
func Setup() {}
