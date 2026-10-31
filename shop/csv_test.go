package shop_test

import (
	"strings"
	"testing"

	"ziniao/shop"
)

// TestParseImportCSV 表格导入：表头可选、脏行不牵连整批、重复行单独记账。
//
// 客户给的表几乎不可能干净——空行、缺名称、重复行都是常态，
// 「有一行脏数据就让整份文件作废」是最容易被运营骂的实现方式。
func TestParseImportCSV(t *testing.T) {
	csvText := "name,platform,site,group\n" +
		"亚马逊美国站01,Amazon,US,北美组\n" +
		",Shopee,MY,东南亚组\n" +
		"\n" +
		"虾皮马来站01,Shopee,MY,\n" +
		"虾皮马来站01,Shopee,MY,\n"
	rows, failures, err := shop.ParseImportCSV(strings.NewReader(csvText), 0)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应解析出 2 行有效数据，实际 %d（%+v）", len(rows), rows)
	}
	if rows[0].Name != "亚马逊美国站01" || rows[0].Group != "北美组" {
		t.Fatalf("首行解析异常: %+v", rows[0])
	}
	if rows[1].Name != "虾皮马来站01" || rows[1].Platform != "Shopee" {
		t.Fatalf("第二行解析异常: %+v", rows[1])
	}
	if len(failures) != 2 {
		t.Fatalf("应有 2 条行级失败，实际 %+v", failures)
	}
	if failures[0].Reason != "缺少店铺名称" || failures[1].Reason != "名称在文件内重复" {
		t.Fatalf("失败原因不可读（要给运营看得懂的话）: %+v", failures)
	}
}

// TestParseImportCSVColumnOrder 列顺序任意：按表头名定位，而不是按位置硬读。
func TestParseImportCSVColumnOrder(t *testing.T) {
	rows, failures, err := shop.ParseImportCSV(strings.NewReader("group,name\n北美组,店A\n"), 0)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "店A" || rows[0].Group != "北美组" {
		t.Fatalf("乱序列解析异常: %+v %+v", rows, failures)
	}
}

// TestParseImportCSVNoHeader 无表头文件时按 name,platform,site,group 顺序解析。
func TestParseImportCSVNoHeader(t *testing.T) {
	rows, failures, err := shop.ParseImportCSV(strings.NewReader("店A,Amazon,US\n店B,Shopee,MY\n"), 0)
	if err != nil {
		t.Fatalf("无表头文件应可解析: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "店A" || rows[1].Site != "MY" {
		t.Fatalf("无表头解析异常: %+v %+v", rows, failures)
	}
}

// TestParseImportCSVLimit 行数上限：超过上限整份拒绝——宁可让运营分批，
// 也不要一次性吃进未知规模的数据（失败原因会被淹没，配额也会被瞬间打满）。
func TestParseImportCSVLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("name\n")
	for i := 0; i < 5; i++ {
		sb.WriteString("店" + string(rune('A'+i)) + "\n")
	}
	if _, _, err := shop.ParseImportCSV(strings.NewReader(sb.String()), 3); err == nil {
		t.Fatal("超过行数上限应整份拒绝")
	}
}

// TestCSVTemplateImportable 模板必须自带表头，且能被解析器直接吃回去（自洽）。
func TestCSVTemplateImportable(t *testing.T) {
	tpl := shop.CSVTemplate()
	if !strings.HasPrefix(tpl, "name,platform,site,group") {
		t.Fatalf("模板表头不符: %q", tpl)
	}
	rows, failures, err := shop.ParseImportCSV(strings.NewReader(tpl), 0)
	if err != nil || len(rows) != 2 || len(failures) != 0 {
		t.Fatalf("模板必须能被直接导入: rows=%d failures=%+v err=%v", len(rows), failures, err)
	}
}
