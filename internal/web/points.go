package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
)

// 点表导入和批量改站号，和桌面版的“导入点表”“修改站号”一样。

// maxImportBytes 是导入点表文件的上限。网关内存有限，比桌面版的 64 MB 小；点表一般不过几 MB。
const maxImportBytes = 16 << 20

// importRequest 是导入点表：浏览器读出文件内容，按 base64 放进 JSON（写操作一律用 JSON，见 secure）。
type importRequest struct {
	Name  string `json:"name"`
	Data  []byte `json:"data"`
	Slave int    `json:"slave"` // 0 表示沿用第一个读取表的站号，没有读取表时用 1
}

type importView struct {
	Format  string   `json:"format"`
	Points  int      `json:"points"`
	Tables  int      `json:"tables"`
	Left    int      `json:"left"` // 超出读取表上限、没建表的范围数
	Slave   int      `json:"slave"`
	Skipped []string `json:"skipped,omitempty"` // 跳过的行和原因
	Notes   []string `json:"notes,omitempty"`
	NoOrder bool     `json:"noOrder,omitempty"` // 文件里没有字节序，32 / 64 位点先按 ABCD
}

// handleImportPoints 解析点表文件（CSV / xlsx，本程序的点表或平台导出的设备属性表），按地址把相近的点合成读取表，
// 替换现有的读取表。解析最多一分钟，浏览器断开时停止。
func (s *Server) handleImportPoints(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "请求格式应为 JSON")
		return
	}
	var in importRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxImportBytes*4/3+4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "点表文件超过 16 MB，或请求内容不对："+err.Error())
		return
	}
	if len(in.Data) == 0 {
		writeError(w, http.StatusBadRequest, "点表文件是空的")
		return
	}
	if in.Slave < 0 || in.Slave > 255 {
		writeError(w, http.StatusBadRequest, "Slave ID 应为 1–255")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	imp, err := points.Parse(ctx, in.Name, in.Data, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "导入点表失败："+err.Error())
		return
	}
	slave := in.Slave
	s.mu.Lock()
	if slave == 0 && len(s.tables) > 0 {
		slave = s.tables[0].cfg.Slave
	}
	s.mu.Unlock()
	if slave == 0 {
		slave = 1
	}
	var cfgs []TableConfig
	for _, rg := range points.Ranges(imp.Points) {
		cfg := TableConfig{Name: "点表 " + modbus.ReferenceSpan(rg.Area, rg.Start, rg.Count), Slave: slave,
			Function: int(rg.Area.ReadFunction()), Address: fmt.Sprintf("0x%04X", rg.Start), Count: rg.Count, IntervalMs: 1000}
		for _, p := range imp.Points {
			if p.Area == rg.Area && p.Offset >= rg.Start && int(p.Offset)+p.Regs() <= int(rg.Start)+rg.Count {
				cfg.Points = append(cfg.Points, configOf(p))
			}
		}
		cfgs = append(cfgs, cfg)
	}
	out := importView{Format: imp.Format, Points: len(imp.Points), Slave: slave, Skipped: imp.Skipped, Notes: imp.Notes, NoOrder: imp.NoOrder}
	var tables []*table
	for _, cfg := range cfgs[:min(len(cfgs), maxTables)] {
		t, err := newTable(cfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, "导入点表失败："+err.Error())
			return
		}
		tables = append(tables, t)
	}
	out.Tables, out.Left = len(tables), len(cfgs)-len(tables)
	s.mu.Lock()
	old := s.tables
	s.tables = nil
	s.mu.Unlock()
	for _, t := range old {
		t.stop()
	}
	for _, t := range tables {
		s.mu.Lock()
		s.tableID++
		t.id = s.tableID
		s.tables = append(s.tables, t)
		s.mu.Unlock()
		s.startTable(t)
	}
	s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "IMPORT", Detail: fmt.Sprintf("导入点表 %s：%d 个点，%d 个读取表，Slave %d", in.Name, out.Points, out.Tables, slave)})
	s.changed()
	writeJSON(w, http.StatusOK, out)
}

// slaveRequest 是批量改站号：把站号是 from 的读取表改成 to，from 为 0 时改全部读取表。
type slaveRequest struct {
	From int `json:"from"`
	To   int `json:"to"`
}

func (s *Server) handleSetSlave(w http.ResponseWriter, r *http.Request) {
	var in slaveRequest
	if !readJSON(w, r, &in) {
		return
	}
	if in.To < 1 || in.To > 255 {
		writeError(w, http.StatusBadRequest, "Slave ID 应为 1–255（0 是广播，读请求没有响应）")
		return
	}
	s.mu.Lock()
	targets := slices.DeleteFunc(slices.Clone(s.tables), func(t *table) bool {
		return t.cfg.Slave == in.To || in.From != 0 && t.cfg.Slave != in.From
	})
	s.mu.Unlock()
	for _, old := range targets {
		cfg := old.cfg
		cfg.Slave = in.To
		t, err := newTable(cfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.replaceTable(old, t)
	}
	s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "SLAVE", Detail: fmt.Sprintf("%d 个读取表改用 Slave %d", len(targets), in.To)})
	s.changed()
	writeJSON(w, http.StatusOK, map[string]int{"changed": len(targets)})
}

// replaceTable 用 t 换掉 old：沿用编号和暂停状态，停掉旧表的轮询，开始新表的。
func (s *Server) replaceTable(old, t *table) {
	old.stop()
	old.mu.Lock()
	t.id, t.paused = old.id, old.paused
	old.mu.Unlock()
	s.mu.Lock()
	if i := slices.Index(s.tables, old); i >= 0 {
		s.tables[i] = t
	}
	s.mu.Unlock()
	s.startTable(t)
}
