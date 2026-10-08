//go:build !windows

package ai

import "errors"

const ProtectedStorage = false

func protect([]byte) ([]byte, error) {
	return nil, errors.New("此平台仅支持会话密钥或 DEEPSEEK_API_KEY 环境变量")
}
func unprotect([]byte) ([]byte, error) {
	return nil, errors.New("此平台不支持 Windows 加密凭据")
}
