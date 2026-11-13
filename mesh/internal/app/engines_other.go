//go:build !windows

package app

// 非 Windows 平台没有桌面客户端适配器（适配器全部 //go:build windows）。
// bot 配置仍可加载（JSON 是跨平台的），但引擎走 unavailableEngine 诚实占位，
// UI 如实显示"适配器未接入"。本仓库的目标平台是 Windows，此文件只为让
// app 包能跨平台编译。

import (
	"mesh/internal/contract"
	"mesh/internal/registry"
)

func buildEngines(bots []contract.BotSpec, _ *registry.Registry) (map[string]contract.Engine, []string) {
	var notes []string
	for _, b := range bots {
		notes = append(notes, "bot \""+b.ID+"\" 在非 Windows 平台没有适配器，将显示为适配器未接入")
	}
	return map[string]contract.Engine{}, notes
}

func detectorFor() registry.Detector { return nil }
