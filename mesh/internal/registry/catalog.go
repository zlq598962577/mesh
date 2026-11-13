package registry

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// 内置 manifest 用 embed.FS 打进二进制。
//
// 为什么要内嵌而不是从安装目录读：内置 agent 是"开箱即用"的承诺，
// 不能依赖安装包把某个目录放到正确位置。内嵌之后，即使安装目录被
// 移动、被清理，四个内置客户端仍然能被识别。
//
// 用户目录（<data_dir>/agents/）是第二层，优先级高于这里：用户可以
// 覆盖内置定义，也可以新增。覆盖规则见 registry.go 的 merge。
//
//go:embed manifests/*.json
var builtinFS embed.FS

// builtinDir 是 embed 里的目录前缀。
const builtinDir = "manifests"

// LoadCatalog 解析全部内置 manifest。
//
// 返回的 map 以 id 为键。任何一个内置 manifest 坏掉都不影响其它：
// 坏的那个放进 invalid，调用方决定怎么展示（通常是在 UI 里标红，
// 但不阻断启动）。这条规则对四层里的每一层都成立。
func LoadCatalog() (valid map[string]*Manifest, invalid []*InvalidManifest, err error) {
	entries, err := fs.ReadDir(builtinFS, builtinDir)
	if err != nil {
		// 内嵌目录读不出来是构建期问题（embed 指令写错），属于编程错误，
		// 直接返回错误而不是静默空表——空表会让人以为"本机没有内置 agent"。
		return nil, nil, fmt.Errorf("registry: 读取内嵌 manifest 目录失败: %w", err)
	}

	valid = make(map[string]*Manifest, len(entries))
	for _, e := range entries {
		if e.IsDir() || !hasManifestExt(e.Name()) {
			continue
		}
		rel := path.Join(builtinDir, e.Name())
		data, readErr := builtinFS.ReadFile(rel)
		if readErr != nil {
			invalid = append(invalid, &InvalidManifest{
				Path:   rel,
				Origin: OriginCatalog,
				Err: &ManifestError{
					Path: rel,
					Msg:  "读取内嵌 manifest 失败",
					Err:  readErr,
				},
			})
			continue
		}
		m, parseErr := ParseManifest(rel, data)
		if parseErr != nil {
			invalid = append(invalid, &InvalidManifest{
				Path:   rel,
				Origin: OriginCatalog,
				Err:    parseErr,
			})
			continue
		}
		m.Origin = OriginCatalog

		// 内置之间重复 id 也是错。这只能是我们自己写坏了，必须暴露出来，
		// 不能"后加载的覆盖先加载的"——那样坏掉的那个会悄悄消失。
		if prev, dup := valid[m.ID]; dup {
			invalid = append(invalid, &InvalidManifest{
				Path:   rel,
				Origin: OriginCatalog,
				Err: &ManifestError{
					Path: rel,
					Msg: fmt.Sprintf("内置 manifest 重复 id %q，与 %s 冲突（两者都不加载）",
						m.ID, prev.Path),
				},
			})
			delete(valid, m.ID)
			continue
		}
		valid[m.ID] = m
	}

	sortInvalid(invalid)
	return valid, invalid, nil
}

// builtinNames 返回内嵌的 manifest 文件名列表，仅用于诊断与测试。
func builtinNames() []string {
	entries, err := fs.ReadDir(builtinFS, builtinDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && hasManifestExt(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// hasManifestExt 判断文件名是否是本实现认的 manifest 扩展名。
//
// .yaml/.yml 也被接受，是为了让方案 §8.2 里写的 "*.yaml" 习惯不至于
// 直接失效——但内容必须是 JSON 语法，见 manifest.go 的包注释。
func hasManifestExt(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".yaml", ".yml":
		return true
	}
	return false
}
