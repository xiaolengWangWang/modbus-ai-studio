//go:build windows

package ai

import (
	"errors"
	"golang.org/x/sys/windows"
	"unsafe"
)

const ProtectedStorage = true

func protect(b []byte) ([]byte, error)   { return crypt(b, false) }
func unprotect(b []byte) ([]byte, error) { return crypt(b, true) }
func crypt(b []byte, decrypt bool) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("凭据为空")
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	var err error
	if decrypt {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, errors.New("Windows 凭据加解密失败，请重新设置 API Key")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	buf := unsafe.Slice(out.Data, int(out.Size))
	copyBuf := append([]byte(nil), buf...)
	clear(buf)
	return copyBuf, nil
}
