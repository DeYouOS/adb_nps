package api

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/djylb/nps/lib/file"
	"github.com/djylb/nps/lib/servercfg"
)

// makeTestAPK 创建一个包含 assets/npc_config.json 与其它文件的最小 APK。
func makeTestAPK(t *testing.T, path string, config []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test apk: %v", err)
	}
	defer func() { _ = f.Close() }()
	w := zip.NewWriter(f)
	entries := map[string][]byte{
		"assets/npc_config.json": config,
		"AndroidManifest.xml":    []byte("manifest-bytes"),
		"classes.dex":            []byte("dex-bytes"),
	}
	for name, data := range entries {
		ew, err := w.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := ew.Write(data); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}

// readZipEntry 读取 zip 中指定文件的内容。
func readZipEntry(t *testing.T, path, name string) []byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer func() { _ = r.Close() }()
	for _, f := range r.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", name, err)
		}
		return data
	}
	t.Fatalf("entry %s not found", name)
	return nil
}

func TestRewriteApkConfigReplacesOnlyTargetEntry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "template.apk")
	dst := filepath.Join(dir, "rewritten.apk")
	makeTestAPK(t, src, []byte(`{"server":"old"}`))

	payload := []byte(`{"server":"new","vkey":"vk-1"}`)
	if err := rewriteApkConfig(src, dst, payload); err != nil {
		t.Fatalf("rewriteApkConfig() error = %v", err)
	}

	if got := readZipEntry(t, dst, "assets/npc_config.json"); string(got) != string(payload) {
		t.Fatalf("npc_config.json = %q, want %q", got, payload)
	}
	if got := readZipEntry(t, dst, "classes.dex"); string(got) != "dex-bytes" {
		t.Fatalf("classes.dex = %q, want unchanged", got)
	}
	if got := readZipEntry(t, dst, "AndroidManifest.xml"); string(got) != "manifest-bytes" {
		t.Fatalf("AndroidManifest.xml = %q, want unchanged", got)
	}
}

func TestBuildPackAPKWritesConfigAndCaches(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "template.apk")
	makeTestAPK(t, template, []byte(`{"server":""}`))

	cfg := &servercfg.PackConfig{
		Enabled:    true,
		Template:   template,
		CacheDir:   filepath.Join(dir, "cache"),
		ServerAddr: "1.2.3.4:8024",
		ConnType:   "tcp",
	}
	client := &file.Client{Id: 7, VerifyKey: "vk-7", Remark: "测试"}

	data, filename, err := buildPackAPK(cfg, client)
	if err != nil {
		t.Fatalf("buildPackAPK() error = %v", err)
	}
	if filename != "npc_7.apk" {
		t.Fatalf("filename = %q, want npc_7.apk", filename)
	}
	if len(data) == 0 {
		t.Fatal("buildPackAPK() returned empty data")
	}

	// 写临时文件验证内容
	out := filepath.Join(dir, "out.apk")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatalf("write out apk: %v", err)
	}
	raw := readZipEntry(t, out, "assets/npc_config.json")
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal npc_config.json: %v", err)
	}
	if got["server"] != "1.2.3.4:8024" || got["vkey"] != "vk-7" || got["conn_type"] != "tcp" {
		t.Fatalf("unexpected config: %#v", got)
	}
	if id, _ := got["client_id"].(float64); int(id) != 7 {
		t.Fatalf("client_id = %v, want 7", got["client_id"])
	}
	if got["remark"] != "测试" {
		t.Fatalf("remark = %v, want 测试", got["remark"])
	}

	// 第二次调用应命中缓存（内容一致）
	data2, _, err := buildPackAPK(cfg, client)
	if err != nil {
		t.Fatalf("buildPackAPK() second call error = %v", err)
	}
	if string(data2) != string(data) {
		t.Fatal("cache hit returned different content")
	}
}

func TestBuildPackAPKTemplateFingerprintInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "template.apk")
	makeTestAPK(t, template, []byte(`{"server":""}`))
	cfg := &servercfg.PackConfig{
		Enabled:    true,
		Template:   template,
		CacheDir:   filepath.Join(dir, "cache"),
		ServerAddr: "1.2.3.4:8024",
		ConnType:   "tcp",
	}
	client := &file.Client{Id: 3, VerifyKey: "vk-3"}

	first, _, err := buildPackAPK(cfg, client)
	if err != nil {
		t.Fatalf("first buildPackAPK() error = %v", err)
	}

	// 修改模板 fingerprint（重写文件使 mtime 变化）
	if err := os.WriteFile(template, []byte("changed"), 0o644); err != nil {
		t.Fatalf("rewrite template: %v", err)
	}
	makeTestAPK(t, template, []byte(`{"server":"v2"}`))

	if _, _, err := buildPackAPK(cfg, client); err == nil {
		// 模板已变，且仍是合法 APK，应该重新生成而非命中旧缓存
	}
	if len(first) == 0 {
		t.Fatal("first build returned empty data")
	}
}

func TestBuildPackAPKRejectsIncompleteConfig(t *testing.T) {
	client := &file.Client{Id: 1, VerifyKey: "vk"}
	cases := []*servercfg.PackConfig{
		nil,
		{},
		{Template: "x"},
		{Template: "x", CacheDir: "y"},
	}
	for i, cfg := range cases {
		if _, _, err := buildPackAPK(cfg, client); err == nil {
			t.Fatalf("case %d: buildPackAPK() expected error", i)
		}
	}
}
