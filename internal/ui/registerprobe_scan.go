package ui

// probeRange 检测读取范围，批量验证正常段，再定位到具体异常地址。
// 和独立检测入口共用检测逻辑，异常 02 才归为非法地址。
func (ws *Workspace) probeRange(w *readWindow) {
	ws.runRegisterProbe(w, w.def)
}
