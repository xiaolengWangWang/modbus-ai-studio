package ui

import (
	"encoding/binary"

	"fyne.io/fyne/v2/dialog"

	"modbus-ai-studio/internal/modbus"
)

// 只读模式：在运行中的设备上排查问题时防止误写。打开后写入对话框、自定义请求里的写类请求、
// 诊断计数器清零都不能用，只放行不会改变设备状态的请求。设置随工作区保存。

// readOnlyPDU 表示只读模式下允许发送的请求：读数据、读状态、读设备标识、诊断里只读的子功能。
// 其他功能码（含设备自定义的）一律不放行，宁可多拦。
func readOnlyPDU(pdu []byte) bool {
	if len(pdu) == 0 {
		return false
	}
	switch modbus.FunctionCode(pdu[0]) {
	case modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs, modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters,
		modbus.FuncReadExceptionStatus, modbus.FuncGetCommEventCounter, modbus.FuncGetCommEventLog, modbus.FuncReportServerID,
		modbus.FuncReadFileRecord, modbus.FuncReadFIFOQueue:
		return true
	case modbus.FuncEncapsulatedInterface:
		return len(pdu) >= 2 && pdu[1] == 0x0E // 读设备标识
	case modbus.FuncDiagnostics:
		if len(pdu) < 3 {
			return false
		}
		sub := binary.BigEndian.Uint16(pdu[1:])
		return sub == 0x00 || sub == 0x02 || (sub >= 0x0B && sub <= 0x12) // 回送、读诊断寄存器、读计数器
	}
	return false
}

func (ws *Workspace) setReadOnly(on bool) {
	ws.readOnly = on
	if ws.readOnlyCheck != nil && ws.readOnlyCheck.Checked != on {
		ws.readOnlyCheck.SetChecked(on)
	}
	if ws.roItem != nil {
		ws.roItem.Checked = on
		if m := ws.win.MainMenu(); m != nil {
			m.Refresh()
		}
	}
	for _, w := range ws.windows {
		w.updateWriteBtn()
	}
	ws.refreshStatus()
}

func (ws *Workspace) showReadOnlyInfo() {
	dialog.ShowInformation("只读模式", "现在是只读模式，不能写入设备。要写入，先在“连接”菜单里关掉“只读模式”。", ws.win)
}
