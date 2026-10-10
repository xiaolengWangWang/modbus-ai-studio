//go:build cgo

package recorder

import (
	"errors"

	"github.com/mattn/go-sqlite3"
)

// 有 cgo 时（桌面版）用 mattn/go-sqlite3。没有 cgo 时见 driver_purego.go，两边的库文件格式相同。
const driverName = "sqlite3"

// writeDSN 是读写连接：外键、WAL（多个进程可以同时写）、忙时最多等 5 秒。
func writeDSN(path string) string {
	return fileURI(path) + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL"
}

// readDSN 是只读连接，查询历史文件时不影响正在写的连接。
func readDSN(path string) string {
	return fileURI(path) + "?mode=ro&_busy_timeout=5000"
}

// isFull 表示文件已到 max_page_count 上限，需要换下一个文件。
func isFull(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) && e.Code == sqlite3.ErrFull
}
