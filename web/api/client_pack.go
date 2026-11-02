package api

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/djylb/nps/lib/file"
	"github.com/djylb/nps/lib/logs"
	"github.com/djylb/nps/lib/servercfg"
	"github.com/djylb/nps/server"
)

// AdbCtlClient 通过 mux 隧道向 NPC 客户端发送 ADB 控制命令（start/stop/restart/reboot）。
func (a *App) AdbCtlClient(c Context) {
	id := requestIntValue(c, "id")
	command := strings.TrimSpace(requestEscapedString(c, "command"))

	if id <= 0 {
		ajax(c, "缺少客户端 ID（id 参数）", 0)
		return
	}
	if command == "" {
		ajax(c, "缺少控制命令（command 参数），可选值：start/stop/restart/reboot", 0)
		return
	}
	switch command {
	case "start", "stop", "restart", "reboot":
	default:
		ajax(c, "不支持的命令: "+command+"，可选值：start/stop/restart/reboot", 0)
		return
	}
	if _, err := a.Services.Clients.Get(id); err != nil {
		ajax(c, "客户端不存在", 0)
		return
	}
	resp, err := server.SendAdbCtl(id, command)
	if err != nil {
		ajax(c, err.Error(), 0)
		return
	}
	status := 0
	if resp.Success {
		status = 1
	}
	ajax(c, resp.Message, status)
}

// PackClient 为指定客户端生成预配置的 APK 安装包并直接下载，支持缓存
// （相同 vkey+server+conn_type 命中缓存直接返回）。
// 配置项见 servercfg.PackConfig。
func (a *App) PackClient(c Context) {
	cfg := a.currentConfig()
	if cfg == nil || !cfg.Pack.Enabled {
		c.RespondString(http.StatusBadRequest, "APK packing is not enabled")
		return
	}
	id := requestIntValue(c, "id")
	if id <= 0 {
		c.RespondString(http.StatusBadRequest, "missing or invalid client id")
		return
	}
	client, err := a.Services.Clients.Get(id)
	if err != nil || client == nil {
		c.RespondString(http.StatusNotFound, "client not found")
		return
	}

	data, filename, err := buildPackAPK(&cfg.Pack, client)
	if err != nil {
		logs.Error("pack: build APK for client %d failed: %v", client.Id, err)
		c.RespondString(http.StatusInternalServerError, err.Error())
		return
	}
	c.SetResponseHeader("Content-Type", "application/vnd.android.package-archive")
	c.SetResponseHeader("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.RespondData(http.StatusOK, "application/vnd.android.package-archive", data)
}

// buildPackAPK 重写模板 APK 的 assets/npc_config.json，按配置对齐/签名后返回 APK 内容。
func buildPackAPK(cfg *servercfg.PackConfig, client *file.Client) ([]byte, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("pack config is nil")
	}
	if cfg.Template == "" || cfg.CacheDir == "" {
		return nil, "", fmt.Errorf("pack config incomplete (pack_template / pack_cache_dir required)")
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create cache dir: %w", err)
	}

	// 缓存 key: sha256(vkey|server|conn_type|template fingerprint)[:16]
	hashInput := client.VerifyKey + "|" + cfg.ServerAddr + "|" + cfg.ConnType
	if fi, err := os.Stat(cfg.Template); err == nil {
		hashInput += "|" + strconv.FormatInt(fi.Size(), 10) + "-" + strconv.FormatInt(fi.ModTime().Unix(), 10)
	} else {
		return nil, "", fmt.Errorf("template APK not found: %s", cfg.Template)
	}
	sum := sha256.Sum256([]byte(hashInput))
	hashStr := hex.EncodeToString(sum[:])[:16]
	filename := fmt.Sprintf("npc_%d.apk", client.Id)
	cachePath := filepath.Join(cfg.CacheDir, fmt.Sprintf("npc_%d_%s.apk", client.Id, hashStr))

	// 命中缓存直接返回
	if fi, err := os.Stat(cachePath); err == nil && fi.Size() > 0 {
		logs.Info("pack cache hit for client %d: %s", client.Id, cachePath)
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return nil, "", fmt.Errorf("read cached apk: %w", err)
		}
		return data, filename, nil
	}

	// 构造 npc_config.json
	cfgData, err := json.Marshal(map[string]interface{}{
		"server":    cfg.ServerAddr,
		"conn_type": cfg.ConnType,
		"vkey":      client.VerifyKey,
		"client_id": client.Id,
		"remark":    client.Remark,
	})
	if err != nil {
		return nil, "", fmt.Errorf("marshal npc config: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "npc_pack_")
	if err != nil {
		return nil, "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// 步骤1: 重写 APK 中的 assets/npc_config.json
	rewrittenApk := filepath.Join(tmpDir, "rewritten.apk")
	if err := rewriteApkConfig(cfg.Template, rewrittenApk, cfgData); err != nil {
		return nil, "", fmt.Errorf("rewrite apk config: %w", err)
	}

	// 步骤2: zipalign（可选）
	alignedApk := rewrittenApk
	if cfg.Zipalign != "" {
		alignedApk = filepath.Join(tmpDir, "aligned.apk")
		out, err := exec.Command(cfg.Zipalign, "-p", "-f", "4", rewrittenApk, alignedApk).CombinedOutput()
		if err != nil {
			return nil, "", fmt.Errorf("zipalign failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	// 步骤3: apksigner 签名（可选）
	signedApk := alignedApk
	if cfg.ApksignerJar != "" && cfg.Keystore != "" {
		signedApk = filepath.Join(tmpDir, "signed.apk")
		out, err := exec.Command("java", "-jar", cfg.ApksignerJar, "sign",
			"--ks", cfg.Keystore,
			"--ks-pass", "pass:"+cfg.KeystorePass,
			"--ks-key-alias", cfg.KeystoreAlias,
			"--key-pass", "pass:"+cfg.KeystorePass,
			"--out", signedApk,
			alignedApk,
		).CombinedOutput()
		if err != nil {
			return nil, "", fmt.Errorf("apksigner sign failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	// 步骤4: 写入缓存目录
	data, err := os.ReadFile(signedApk)
	if err != nil {
		return nil, "", fmt.Errorf("read signed apk: %w", err)
	}
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		logs.Warn("pack: write cache failed: %v", err)
	}
	logs.Info("pack: generated APK for client %d, cached at %s", client.Id, cachePath)
	return data, filename, nil
}

// rewriteApkConfig 重写 APK 中的 assets/npc_config.json，其他文件原样复制。
func rewriteApkConfig(src, dst string, configData []byte) error {
	srcZip, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcZip.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	writer := zip.NewWriter(out)
	defer func() { _ = writer.Close() }()

	for _, f := range srcZip.File {
		// 保留原文件头信息（时间戳、权限）
		header := f.FileHeader
		if f.Name == "assets/npc_config.json" {
			header.Method = zip.Deflate
		}
		w, err := writer.CreateHeader(&header)
		if err != nil {
			return err
		}
		if f.Name == "assets/npc_config.json" {
			if _, err := w.Write(configData); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		_, err = io.Copy(w, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
