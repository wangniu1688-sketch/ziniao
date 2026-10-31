package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// postCSVUpload 以 multipart/form-data 上传 CSV 文件，返回状态码与 JSON body。
func postCSVUpload(t *testing.T, url, token, filename, content string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("响应不是 JSON: %v（原文 %s）", err, string(body))
		}
	}
	return resp.StatusCode, out
}

// TestBatchImportCSVE2E 端到端：上传 CSV → dry-run 不落数据 → 真导入部分成功返回 207 → 模板可下载。
func TestBatchImportCSVE2E(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "导入团队", "csv_owner")

	// 1) 模板必须能直接下载下来给运营填
	st, body := do(t, "GET", srv.URL+"/api/v1/shops/batch/template", token, nil)
	if st != http.StatusOK {
		t.Fatalf("模板下载失败: %d %v", st, body)
	}
	template := httpGetRaw(t, srv.URL+"/api/v1/shops/batch/template", token)
	if !strings.Contains(template, "name,platform,site,group") {
		t.Fatalf("模板内容异常: %q", template)
	}

	// 2) dry-run：只体检不写库，用来在真正导入前先看数据干不干净
	csvText := "name,platform,site,group\n" +
		"CSV美国站01,Amazon,US,北美组\n" +
		",Shopee,MY,东南亚组\n" // 缺名称，应被判失败
	st, body = postCSVUpload(t, srv.URL+"/api/v1/shops/batch/import?dry_run=1", token, "shops.csv", csvText)
	if st != http.StatusOK {
		t.Fatalf("dry-run 失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Fatalf("dry-run 应统计 2 行，实际 %v", total)
	}
	if created, _ := body["created"].(float64); created != 0 {
		t.Fatalf("dry-run 不得创建数据，实际创建 %v", created)
	}
	if failed, _ := body["failed"].(float64); failed != 1 {
		t.Fatalf("dry-run 应报出 1 行失败，实际 %v（%v）", failed, body)
	}

	// dry-run 之后库里必须还是空的
	st, body = do(t, "GET", srv.URL+"/api/v1/shops?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("查询店铺失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Fatalf("dry-run 不得写入数据，实际有 %v 家店铺", total)
	}

	// 3) 真导入：有效行入库，脏行计入 failures，整体返回 207
	st, body = postCSVUpload(t, srv.URL+"/api/v1/shops/batch/import", token, "shops.csv", csvText)
	if st != http.StatusMultiStatus {
		t.Fatalf("部分成功应返回 207，实际 %d %v", st, body)
	}
	if created, _ := body["created"].(float64); created != 1 {
		t.Fatalf("应成功导入 1 家，实际 %v", created)
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Fatalf("总数应把失败行算进去（否则用户会以为那一行被静默丢掉了），实际 %v", total)
	}

	// 4) 行数封顶：整份拒绝，而不是悄悄只导前 N 行
	var sb strings.Builder
	sb.WriteString("name\n")
	for i := 0; i < 1200; i++ {
		sb.WriteString("店" + string(rune('A'+i%26)) + "\n")
	}
	if st, body = postCSVUpload(t, srv.URL+"/api/v1/shops/batch/import", token, "big.csv", sb.String()); st == http.StatusOK {
		t.Fatalf("超量文件应被拒绝: %v", body)
	}
}
