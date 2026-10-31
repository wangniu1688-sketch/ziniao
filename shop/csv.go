package shop

import (
	"encoding/csv"
	"errors"
	"io"
	"strings"

	"ziniao/platform/apierr"
)

// MaxImportRows 单次导入行数上限（含表头在内的数据行数）。
//
// 设上限的理由不是性能，而是操作安全：一次性吃进几万行的 CSV 时，
// 失败原因会淹没日志、配额会被瞬间打满，事后很难还原「到底导进来了什么」。
const MaxImportRows = 1000

// ErrTooManyRows 行数超过上限。
var ErrTooManyRows = errors.New("导入行数超过单次上限")

// ParseImportCSV 解析 CSV 文件为待导入行。
//
// 表头可选：首行里出现 "name" 就按表头解析（列顺序任意），否则按
// name,platform,site,group 固定顺序解析。兼容 Excel 导出的 UTF-8 BOM。
//
// 行级问题（空行、缺名称）不中断整批处理——这是这一行的初衷：
// 客户给的表里有一两行脏数据很正常，不该让整份文件作废。
func ParseImportCSV(r io.Reader, maxRows int) ([]BatchImportRow, []BatchFailure, error) {
	if maxRows <= 0 {
		maxRows = MaxImportRows
	}
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // 允许每行列数不同，缺列由下面的校验给出可读原因
	raw, err := reader.ReadAll()
	if err != nil {
		return nil, nil, apierr.Validation("CSV 解析失败：" + err.Error())
	}

	nameIdx, platformIdx, siteIdx, groupIdx := 0, 1, 2, 3
	if len(raw) > 0 && hasHeader(raw[0]) {
		nameIdx, platformIdx, siteIdx, groupIdx = columnIndex(raw[0])
		raw = raw[1:]
	}
	if len(raw) > maxRows {
		return nil, nil, apierr.Validation(ErrTooManyRows.Error() + "（最大 " + itoaSimple(maxRows) + " 行）")
	}

	rows := make([]BatchImportRow, 0, len(raw))
	failures := []BatchFailure{}
	seen := map[string]bool{}
	for i, rec := range raw {
		lineNo := i + 1
		if isBlankRecord(rec) {
			continue // 空行直接忽略，不算失败
		}
		name := strings.TrimSpace(cell(rec, nameIdx))
		if name == "" {
			failures = append(failures, BatchFailure{Row: lineNo, Reason: "缺少店铺名称"})
			continue
		}
		if seen[name] {
			failures = append(failures, BatchFailure{Row: lineNo, Name: name, Reason: "名称在文件内重复"})
			continue
		}
		seen[name] = true
		rows = append(rows, BatchImportRow{
			Name:     name,
			Platform: strings.TrimSpace(cell(rec, platformIdx)),
			Site:     strings.TrimSpace(cell(rec, siteIdx)),
			Group:    strings.TrimSpace(cell(rec, groupIdx)),
		})
	}
	return rows, failures, nil
}

// CSVTemplate 返回导入模板（含示例行，Excel 可直接打开）。
func CSVTemplate() string {
	return "name,platform,site,group\n亚马逊美国站01,Amazon,US,北美组\n虾皮马来站01,Shopee,MY,\n"
}

func hasHeader(rec []string) bool {
	for _, c := range rec {
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(c, "\ufeff")), "name") {
			return true
		}
	}
	return false
}

// columnIndex 按表头名定位各列；缺失的列用 -1（取空值）。
func columnIndex(header []string) (name, platform, site, group int) {
	name, platform, site, group = -1, -1, -1, -1
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		switch key {
		case "name", "店铺名称", "名称":
			if name < 0 {
				name = i
			}
		case "platform", "平台":
			if platform < 0 {
				platform = i
			}
		case "site", "站点":
			if site < 0 {
				site = i
			}
		case "group", "分组":
			if group < 0 {
				group = i
			}
		}
	}
	if name < 0 {
		name = 0
	}
	return name, platform, site, group
}

func cell(rec []string, idx int) string {
	if idx < 0 || idx >= len(rec) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(rec[idx]), "\ufeff")
}

func isBlankRecord(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func itoaSimple(i int) string {
	if i <= 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}
