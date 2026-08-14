// keadm 产物校验清单（M4C P2-② 修复）。
//
// 背景：reset 按文件名白名单删除产物，若用户恰好放了同名文件
// （如自己的 install.sh），会被误删。修复方案：init/join 每次生成产物时
// 记录「文件名 → sha256」到 keadm-manifest.json；reset 删除前先校验
// 当前文件哈希与清单一致才删，不匹配（被用户修改/替换）则跳过并提示
// 人工确认。旧版本产物（无清单）保持原有白名单行为（确认后删除），
// 并提示无校验保护（建议重新生成产物以获得校验能力）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// manifestName 是校验清单文件名（keadm 自身的账本文件，位于产物目录内）。
const manifestName = "keadm-manifest.json"

// manifest 是校验清单：文件名 → sha256 十六进制（小写）。
type manifest struct {
	Files map[string]string `json:"files"`
}

// loadManifest 读取产物目录下的校验清单。
// 返回 (清单, 清单文件是否存在, 错误)：清单不存在时返回空清单 + exists=false
// （调用方据此走旧版行为）；存在但解析失败时返回错误（保守处理，见 runReset）。
func loadManifest(dir string) (*manifest, bool, error) {
	path := filepath.Join(dir, manifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &manifest{Files: map[string]string{}}, false, nil
		}
		return nil, false, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, true, fmt.Errorf("校验清单 %s 解析失败: %w", manifestName, err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, true, nil
}

// saveManifest 把校验清单写回产物目录（0644）。
func saveManifest(dir string, m *manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(dir, manifestName), b, 0o644)
}

// recordGeneratedFiles 把刚生成的产物文件登记进校验清单（先算 sha256 再落清单）。
func recordGeneratedFiles(dir string, names []string, m *manifest) error {
	for _, name := range names {
		sum, err := sha256File(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("计算 %s 校验和失败: %w", name, err)
		}
		m.Files[name] = sum
	}
	return nil
}

// sha256File 计算文件内容的 sha256（十六进制小写）。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyManifestFile 校验产物目录内文件哈希与清单记录一致。
// 返回 ok=false 表示清单未登记该文件或哈希不匹配（文件很可能不是
// keadm 生成的，或已被用户修改/替换——reset 应跳过并提示人工确认）。
func verifyManifestFile(dir, name string, m *manifest) (bool, error) {
	want, ok := m.Files[name]
	if !ok {
		return false, nil
	}
	got, err := sha256File(filepath.Join(dir, name))
	if err != nil {
		return false, err
	}
	return got == want, nil
}
