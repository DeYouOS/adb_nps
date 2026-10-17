package server

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/beego/beego"
	"github.com/djylb/nps/lib/file"
	"github.com/djylb/nps/lib/logs"
)

var proxyPoolMu sync.Mutex

// ProxyEntry represents a single proxy in the pool
type ProxyEntry struct {
	ClientId int    `json:"client_id"`
	Vkey     string `json:"vkey"`
	Port     int    `json:"port"`
	Mode     string `json:"mode"` // "socks5" or "httpProxy"
	Remark   string `json:"remark"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	ProxyURL string `json:"proxy_url"` // e.g., "socks5://user:pass@ip:10801" or "http://user:pass@ip:8080"
}

// getProxyPoolPath returns the configured proxy pool file path
func getProxyPoolPath() string {
	path := beego.AppConfig.String("socks5_proxy_pool_file")
	if path == "" {
		path = "/tmp/nps_proxy_pool.json"
	}
	return path
}

// UpdateProxyPoolFile regenerates the proxy pool file from running SOCKS5 and HTTP proxy tunnels
func UpdateProxyPoolFile() {
	proxyPoolMu.Lock()
	defer proxyPoolMu.Unlock()

	var entries []ProxyEntry
	RunList.Range(func(key, value interface{}) bool {
		t, err := file.GetDb().GetTask(key.(int))
		if err != nil || !t.Status {
			return true
		}
		if t.Mode != "socks5" && t.Mode != "httpProxy" {
			return true
		}
		entry := ProxyEntry{
			ClientId: t.Client.Id,
			Vkey:     t.Client.VerifyKey,
			Port:     t.Port,
			Mode:     t.Mode,
			Remark:   t.Remark,
		}
		// Extract credentials from UserAuth
		if t.UserAuth != nil && len(t.UserAuth.AccountMap) > 0 {
			for u, p := range t.UserAuth.AccountMap {
				entry.Username = u
				entry.Password = p
				break
			}
		}
		// Use configured external IP for proxy URLs
		proxyIP := beego.AppConfig.DefaultString("socks5_proxy_pool_external_ip", "0.0.0.0")
		// Build proxy URL with auth if available
		scheme := "socks5"
		if t.Mode == "httpProxy" {
			scheme = "http"
		}
		if entry.Username != "" {
			entry.ProxyURL = fmt.Sprintf("%s://%s:%s@%s:%d", scheme, entry.Username, entry.Password, proxyIP, t.Port)
		} else {
			entry.ProxyURL = fmt.Sprintf("%s://%s:%d", scheme, proxyIP, t.Port)
		}
		entries = append(entries, entry)
		return true
	})

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		logs.Error("Failed to marshal proxy pool: %v", err)
		return
	}
	if err := os.WriteFile(getProxyPoolPath(), data, 0644); err != nil {
		logs.Error("Failed to write proxy pool file: %v", err)
		return
	}
	logs.Info("Updated proxy pool file %s with %d entries", getProxyPoolPath(), len(entries))
}
