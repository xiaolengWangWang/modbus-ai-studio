package ui

import (
	"context"

	"modbus-ai-studio/internal/probe"
)

// 寄存器检测的算法、结果文字和报告在 internal/probe，modbus-cli 共用；界面代码沿用这里的短名字，
// 读取定义用 probeDef 换成检测范围。

type (
	probeRun     = probe.Run
	probeSegment = probe.Segment
	probeMode    = probe.Mode
	pointCheck   = probe.PointCheck
	quickResult  = probe.QuickResult
	probeOutput  = probe.Output
	probeLog     = probe.Log
	probeClient  = probe.Client
	loggedProbe  = probe.Logged
	jsonlEntry   = probe.JSONLEntry
)

const (
	probeAuto       = probe.ModeAuto
	probeSingle     = probe.ModeSingle
	probePoints     = probe.ModePoints
	probeEachPoint  = probe.ModeEachPoint
	probeQuick      = probe.ModeQuick
	quickBudget     = probe.QuickBudget
	quickCandidates = probe.QuickCandidates
	illegalNote     = probe.IllegalNote
	phaseBatch      = probe.PhaseBatch
	phasePoint      = probe.PhasePoint
)

var (
	probeModeNames    = probe.ModeNames
	newProbeLog       = probe.NewLog
	exportProbeReport = probe.ExportReport
)

// probeDef 是读取定义对应的检测范围。
func probeDef(d readDef) probe.Def {
	return probe.Def{Slave: d.Slave, Function: d.Function, Start: d.Start, Qty: d.Qty}
}

// readDefOf 把检测范围套回读取定义：站号、功能码、起止取自 p，扫描周期、显示格式等沿用 base。
func readDefOf(base readDef, p probe.Def) readDef {
	base.Slave, base.Function, base.Start, base.Qty = p.Slave, p.Function, p.Start, p.Qty
	return base
}

func pointProbeDef(pts pointTable, d readDef) (readDef, []probeSegment, bool) {
	pd, segs, ok := probe.PointDef(pts, probeDef(d))
	return readDefOf(d, pd), segs, ok
}

func probeLocate(ctx context.Context, c probeClient, d readDef, progress func(int), recheck ...func(int)) ([]int8, error) {
	return probe.Locate(ctx, c, probeDef(d), progress, recheck...)
}

func probeUnits(ctx context.Context, c probeClient, d readDef, mode probeMode, segs []probeSegment, progress func(int)) ([]int8, error) {
	return probe.Units(ctx, c, probeDef(d), mode, segs, progress)
}

func checkEachPoint(ctx context.Context, c probeClient, d readDef, segs []probeSegment, progress func(int)) ([]pointCheck, error) {
	return probe.CheckEachPoint(ctx, c, probeDef(d), segs, progress)
}

func pointCheckStates(d readDef, checks []pointCheck) []int8 { return probe.PointCheckStates(probeDef(d), checks) }

func quickCheckRange(ctx context.Context, c probeClient, d readDef, pts pointTable, budget int, progress func(int)) (*quickResult, error) {
	return probe.QuickCheck(ctx, c, probeDef(d), pts, budget, progress)
}

func quickUnits(pts pointTable, d readDef) []probe.QuickUnit { return probe.QuickUnits(pts, probeDef(d)) }

func quickText(d readDef, q *quickResult, stopErr error) probeOutput {
	return probe.QuickText(probeDef(d), q, stopErr)
}

func quickStates(d readDef, q *quickResult) []int8 { return probe.QuickStates(probeDef(d), q) }

func quickEvidence(d readDef, q *quickResult) map[string]any { return probe.QuickEvidence(probeDef(d), q) }

func probeOutputFor(d readDef, res []int8, stopErr error, segs ...probeSegment) (probeOutput, int, int) {
	return probe.OutputFor(probeDef(d), res, stopErr, segs...)
}

func eachPointOutput(d readDef, checks []pointCheck, segs []probeSegment, stopErr error) probeOutput {
	return probe.EachPointOutput(probeDef(d), checks, segs, stopErr)
}

func pointProbeText(d readDef, res []int8, segs []probeSegment) (text, summary string, bad int) {
	return probe.PointText(probeDef(d), res, segs)
}

func eachPointText(d readDef, checks []pointCheck, segs []probeSegment) (text, summary string, bad int) {
	return probe.EachPointText(probeDef(d), checks, segs)
}
