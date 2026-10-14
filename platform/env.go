package platform

import (
	"os"
	"strings"
	"sync"
)

// Windows 兼容版把本进程的 APPDATA、LOCALAPPDATA 改到程序目录（windows.go 的 portable），
// exe 旁边有 Mesa 时还设了 GALLIUM_DRIVER。这些只该影响本进程：启动浏览器、资源管理器、
// 更新后的新版本时用 ChildEnv 换回原值，免得浏览器把自己的配置建到程序目录里。

var (
	envMu    sync.Mutex
	envSaved = map[string]*string{} // 改过的变量的原值，nil 表示原来没有
)

// setenv 设置本进程的环境变量，第一次改某个变量时记下原值。
func setenv(key, value string) error {
	envMu.Lock()
	defer envMu.Unlock()
	if _, ok := envSaved[key]; !ok {
		if v, ok := os.LookupEnv(key); ok {
			envSaved[key] = &v
		} else {
			envSaved[key] = nil
		}
	}
	return os.Setenv(key, value)
}

// ChildEnv 是启动别的程序用的环境变量：本进程改过的变量换回原值。没改过时返回 nil，即和本进程相同。
func ChildEnv() []string {
	envMu.Lock()
	defer envMu.Unlock()
	if len(envSaved) == 0 {
		return nil
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !savedKey(k) {
			env = append(env, kv)
		}
	}
	for k, v := range envSaved {
		if v != nil {
			env = append(env, k+"="+*v)
		}
	}
	return env
}

// savedKey 判断 k 是不是改过的变量。Windows 的变量名不分大小写。
func savedKey(k string) bool {
	for s := range envSaved {
		if strings.EqualFold(s, k) {
			return true
		}
	}
	return false
}
