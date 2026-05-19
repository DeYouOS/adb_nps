package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/beego/beego"
	"github.com/djylb/nps/lib/file"
	"github.com/djylb/nps/lib/logs"
)

var proxyPoolMu sync.Mutex

// ProxyEntry represents a single SOCKS5 proxy in the pool
type ProxyEntry struct {
	ClientId int    `json:"client_id"`
	Port     int    `json:"port"`
	Remark   string `json:"remark"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	ProxyURL string `json:"proxy_url"` // e.g., "socks5://user:pass@ip:10801"
}

// getProxyPoolPath returns the configured proxy pool file path
func getProxyPoolPath() string {
	path := beego.AppConfig.String("socks5_proxy_pool_file")
	if path == "" {
		path = "/tmp/nps_proxy_pool.json"
	}
	return path
}

// UpdateProxyPoolFile regenerates the proxy pool file from running SOCKS5 tunnels
func UpdateProxyPoolFile() {
	proxyPoolMu.Lock()
	defer proxyPoolMu.Unlock()

	var entries []ProxyEntry
	RunList.Range(func(key, value interface{}) bool {
		if t, err := file.GetDb().GetTask(key.(int)); err == nil && t.Mode == "socks5" && t.Status {
			entry := ProxyEntry{
				ClientId: t.Client.Id,
				Port:     t.Port,
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
			// Build proxy URL with auth if available
			if entry.Username != "" {
				entry.ProxyURL = fmt.Sprintf("socks5://%s:%s@0.0.0.0:%d", entry.Username, entry.Password, t.Port)
			} else {
				entry.ProxyURL = "socks5://0.0.0.0:" + strconv.Itoa(t.Port)
			}
			entries = append(entries, entry)
		}
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
