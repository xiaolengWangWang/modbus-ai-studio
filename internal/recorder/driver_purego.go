//go:build !cgo

package recorder

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// 没有 cgo 时（Linux Web 版静态编译、交叉编译）用纯 Go 的 modernc.org/sqlite，库文件格式与桌面版相同。
const driverName = "sqlite"

// writeDSN 是读写连接：外键、WAL（多个进程可以同时写）、忙时最多等 5 秒。
func writeDSN(path string) string {
	return fileURI(path) + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
}

// readDSN 是只读连接，查询历史文件时不影响正在写的连接。
func readDSN(path string) string {
	return fileURI(path) + "?mode=ro&_pragma=busy_timeout(5000)"
}

// isFull 表示文件已到 max_page_count 上限，需要换下一个文件。
func isFull(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_FULL
}
