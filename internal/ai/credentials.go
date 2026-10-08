package ai

import (
	"errors"
	"os"
	"path/filepath"
)

func KeyPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("无法确定用户配置目录")
	}
	return filepath.Join(dir, "ModbusAIStudio", "deepseek.key"), nil
}
func SaveKey(path, key string) error {
	if key == "" {
		return DeleteKey(path)
	}
	if len(key) > 4096 {
		return errors.New("API Key 长度无效")
	}
	b, err := protect([]byte(key))
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("无法创建凭据目录")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".deepseek-key-*")
	if err != nil {
		return errors.New("无法保存加密凭据")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("写入加密凭据失败")
	}
	if os.Rename(name, path) != nil {
		return errors.New("替换加密凭据失败")
	}
	return nil
}
func LoadKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || len(b) > 16384 || len(b) == 0 {
		return "", errors.New("无法读取加密凭据，请重新设置 API Key")
	}
	p, err := unprotect(b)
	if err != nil {
		return "", err
	}
	defer clear(p)
	return string(p), nil
}
func DeleteKey(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("删除凭据失败")
	}
	return nil
}
