package server

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/beego/beego"
	"github.com/djylb/nps/bridge"
	"github.com/djylb/nps/lib/common"
	"github.com/djylb/nps/lib/file"
	"github.com/djylb/nps/lib/logs"
	"github.com/djylb/nps/lib/version"
	"github.com/djylb/nps/server/proxy"
	"github.com/djylb/nps/server/tool"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

var (
	Bridge  *bridge.Bridge
	RunList sync.Map //map[int]interface{}
	once    sync.Once
)

func init() {
	RunList = sync.Map{}
}

// init task from db
func InitFromCsv() {
	//Add a public password
	if vkey := beego.AppConfig.String("public_vkey"); vkey != "" {
		c := file.NewClient(vkey, true, true)
		file.GetDb().NewClient(c)
		RunList.Store(c.Id, nil)
		//RunList[c.Id] = nil
	}
	//Initialize services in server-side files
	file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
		if value.(*file.Tunnel).Status {
			AddTask(value.(*file.Tunnel))
		}
		return true
	})
}

// get bridge command
func DealBridgeTask() {
	for {
		select {
		case t := <-Bridge.OpenTask:
			AddTask(t)
		case t := <-Bridge.CloseTask:
			StopServer(t.Id)
		case id := <-Bridge.CloseClient:
			DelTunnelAndHostByClientId(id, true)
			if v, ok := file.GetDb().JsonDb.Clients.Load(id); ok {
				if v.(*file.Client).NoStore {
					file.GetDb().DelClient(id)
				}
			}
		case tunnel := <-Bridge.OpenTask:
			StartTask(tunnel.Id)
		case s := <-Bridge.SecretChan:
			logs.Trace("New secret connection, addr %v", s.Conn.Conn.RemoteAddr())
			if t := file.GetDb().GetTaskByMd5Password(s.Password); t != nil {
				if t.Status {
					go proxy.NewBaseServer(Bridge, t).DealClient(s.Conn, t.Client, t.Target.TargetStr, nil, common.CONN_TCP, nil, []*file.Flow{t.Flow, t.Client.Flow}, t.Target.ProxyProtocol, t.Target.LocalProxy, t)
				} else {
					s.Conn.Close()
					logs.Trace("This key %s cannot be processed,status is close", s.Password)
				}
			} else {
				logs.Trace("This key %s cannot be processed", s.Password)
				s.Conn.Close()
			}
		}
	}
}

// start a new server
func StartNewServer(bridgePort int, cnf *file.Tunnel, bridgeType string, bridgeDisconnect int) {
	Bridge = bridge.NewTunnel(bridgePort, bridgeType, common.GetBoolByStr(beego.AppConfig.String("ip_limit")), &RunList, bridgeDisconnect)
	go func() {
		if err := Bridge.StartTunnel(); err != nil {
			logs.Error("start server bridge error %v", err)
			os.Exit(0)
		}
	}()
	if p, err := beego.AppConfig.Int("p2p_port"); err == nil {
		for i := 0; i < 3; i++ {
			port := p + i
			if common.TestUdpPort(port) {
				go proxy.NewP2PServer(port).Start()
				logs.Info("Started P2P Server on port %d", port)
			} else {
				logs.Error("Port %d is unavailable.", port)
			}
		}
	}
	Bridge.OnClientConnect = AutoCreateSocks5ForClient
	go DealBridgeTask()
	go dealClientFlow()
	if svr := NewMode(Bridge, cnf); svr != nil {
		if err := svr.Start(); err != nil {
			logs.Error("%v", err)
		}
		RunList.Store(cnf.Id, svr)
		//RunList[cnf.Id] = svr
	} else {
		logs.Error("Incorrect startup mode %s", cnf.Mode)
	}
}

func dealClientFlow() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			dealClientData()
		}
	}
}

// new a server by mode name
func NewMode(Bridge *bridge.Bridge, c *file.Tunnel) proxy.Service {
	var service proxy.Service
	switch c.Mode {
	case "tcp", "file":
		service = proxy.NewTunnelModeServer(proxy.ProcessTunnel, Bridge, c)
	case "socks5":
		service = proxy.NewSock5ModeServer(Bridge, c)
	case "httpProxy":
		service = proxy.NewTunnelModeServer(proxy.ProcessHttp, Bridge, c)
	case "tcpTrans":
		service = proxy.NewTunnelModeServer(proxy.HandleTrans, Bridge, c)
	case "udp":
		service = proxy.NewUdpModeServer(Bridge, c)
	case "webServer":
		InitFromCsv()
		t := &file.Tunnel{
			Port:   0,
			Mode:   "httpHostServer",
			Status: true,
		}
		AddTask(t)
		service = proxy.NewWebServer(Bridge)
	case "httpHostServer":
		httpPort, _ := beego.AppConfig.Int("http_proxy_port")
		httpsPort, _ := beego.AppConfig.Int("https_proxy_port")
		//useCache, _ := beego.AppConfig.Bool("http_cache")
		//cacheLen, _ := beego.AppConfig.Int("http_cache_length")
		addOrigin, _ := beego.AppConfig.Bool("http_add_origin_header")
		httpOnlyPass := beego.AppConfig.String("x_nps_http_only")
		service = proxy.NewHttp(Bridge, c, httpPort, httpsPort, httpOnlyPass, addOrigin)
	}
	return service
}

// stop server
func StopServer(id int) error {
	//if v, ok := RunList[id]; ok {
	if v, ok := RunList.Load(id); ok {
		if svr, ok := v.(proxy.Service); ok {
			if err := svr.Close(); err != nil {
				return err
			}
			logs.Info("stop server id %d", id)
		} else {
			logs.Warn("stop server id %d error", id)
		}
		if t, err := file.GetDb().GetTask(id); err != nil {
			return err
		} else {
			t.Status = false
			logs.Info("close port %d,remark %s,client id %d,task id %d", t.Port, t.Remark, t.Client.Id, t.Id)
			file.GetDb().UpdateTask(t)
		}
		//delete(RunList, id)
		RunList.Delete(id)
		return nil
	}
	return errors.New("task is not running")
}

// add task
func AddTask(t *file.Tunnel) error {
	if t.Mode == "secret" || t.Mode == "p2p" {
		logs.Info("secret task %s start ", t.Remark)
		//RunList[t.Id] = nil
		RunList.Store(t.Id, nil)
		return nil
	}
	if b := tool.TestServerPort(t.Port, t.Mode); !b && t.Mode != "httpHostServer" {
		logs.Error("taskId %d start error port %d open failed", t.Id, t.Port)
		return errors.New("the port open error")
	}
	if minute, err := beego.AppConfig.Int("flow_store_interval"); err == nil && minute > 0 {
		go flowSession(time.Minute * time.Duration(minute))
	}
	if svr := NewMode(Bridge, t); svr != nil {
		logs.Info("tunnel task %s start mode：%s port %d", t.Remark, t.Mode, t.Port)
		//RunList[t.Id] = svr
		RunList.Store(t.Id, svr)
		go func() {
			if err := svr.Start(); err != nil {
				logs.Error("clientId %d taskId %d start error %v", t.Client.Id, t.Id, err)
				//delete(RunList, t.Id)
				RunList.Delete(t.Id)
				return
			}
		}()
	} else {
		return errors.New("the mode is not correct")
	}
	return nil
}

// start task
func StartTask(id int) error {
	if t, err := file.GetDb().GetTask(id); err != nil {
		return err
	} else {
		AddTask(t)
		t.Status = true
		file.GetDb().UpdateTask(t)
	}
	return nil
}

// delete task
func DelTask(id int) error {
	//if _, ok := RunList[id]; ok {
	if _, ok := RunList.Load(id); ok {
		if err := StopServer(id); err != nil {
			return err
		}
	}
	return file.GetDb().DelTask(id)
}

// get task list by page num
func GetTunnel(start, length int, typeVal string, clientId int, search string, sortField string, order string) ([]*file.Tunnel, int) {
	all_list := make([]*file.Tunnel, 0) //store all Tunnel
	list := make([]*file.Tunnel, 0)
	originLength := length
	var cnt int
	keys := file.GetMapKeys(file.GetDb().JsonDb.Tasks, false, "", "")

	//get all Tunnel and sort
	for _, key := range keys {
		if value, ok := file.GetDb().JsonDb.Tasks.Load(key); ok {
			v := value.(*file.Tunnel)
			if (typeVal != "" && v.Mode != typeVal || (clientId != 0 && v.Client.Id != clientId)) || (typeVal == "" && clientId != v.Client.Id) {
				continue
			}
			all_list = append(all_list, v)
		}
	}
	//sort by Id, Remark, TargetStr, Port, asc or desc
	if sortField == "Id" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Id < all_list[j].Id })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Id > all_list[j].Id })
		}
	} else if sortField == "Client.Id" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Client.Id < all_list[j].Client.Id })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Client.Id > all_list[j].Client.Id })
		}
	} else if sortField == "Remark" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Remark < all_list[j].Remark })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Remark > all_list[j].Remark })
		}
	} else if sortField == "Client.VerifyKey" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Client.VerifyKey < all_list[j].Client.VerifyKey })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Client.VerifyKey > all_list[j].Client.VerifyKey })
		}
	} else if sortField == "Target.TargetStr" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Target.TargetStr < all_list[j].Target.TargetStr })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Target.TargetStr > all_list[j].Target.TargetStr })
		}
	} else if sortField == "Port" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Port < all_list[j].Port })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Port > all_list[j].Port })
		}
	} else if sortField == "Mode" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Mode < all_list[j].Mode })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Mode > all_list[j].Mode })
		}
	} else if sortField == "Password" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Password < all_list[j].Password })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Password > all_list[j].Password })
		}
	} else if sortField == "Status" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Status && !all_list[j].Status })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return !all_list[i].Status && all_list[j].Status })
		}
	} else if sortField == "RunStatus" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].RunStatus && !all_list[j].RunStatus })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return !all_list[i].RunStatus && all_list[j].RunStatus })
		}
	} else if sortField == "Client.IsConnect" {
		if order == "asc" {
			sort.SliceStable(all_list, func(i, j int) bool { return all_list[i].Client.IsConnect && !all_list[j].Client.IsConnect })
		} else {
			sort.SliceStable(all_list, func(i, j int) bool { return !all_list[i].Client.IsConnect && all_list[j].Client.IsConnect })
		}
	}

	//search
	for _, key := range all_list {
		if value, ok := file.GetDb().JsonDb.Tasks.Load(key.Id); ok {
			v := value.(*file.Tunnel)
			if (typeVal != "" && v.Mode != typeVal || (clientId != 0 && v.Client.Id != clientId)) || (typeVal == "" && clientId != v.Client.Id) {
				continue
			}
			if search != "" && !(v.Id == common.GetIntNoErrByStr(search) || v.Port == common.GetIntNoErrByStr(search) || common.ContainsFold(v.Password, search) || common.ContainsFold(v.Remark, search) || common.ContainsFold(v.Target.TargetStr, search)) {
				continue
			}
			cnt++
			if _, ok := Bridge.Client.Load(v.Client.Id); ok {
				v.Client.IsConnect = true
			} else {
				v.Client.IsConnect = false
			}
			if start--; start < 0 {
				if originLength == 0 {
					if _, ok := RunList.Load(v.Id); ok {
						v.RunStatus = true
					} else {
						v.RunStatus = false
					}
					list = append(list, v)
				} else if length--; length >= 0 {
					//if _, ok := RunList[v.Id]; ok {
					if _, ok := RunList.Load(v.Id); ok {
						v.RunStatus = true
					} else {
						v.RunStatus = false
					}
					list = append(list, v)
				}
			}
		}
	}
	return list, cnt
}

// get client list
func GetHostList(start, length, clientId int, search, sortField, order string) (list []*file.Host, cnt int) {
	list, cnt = file.GetDb().GetHost(start, length, clientId, search)
	//sort by Id, Remark..., asc or desc
	if sortField == "Id" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Id < list[j].Id })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Id > list[j].Id })
		}
	} else if sortField == "Client.Id" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Client.Id < list[j].Client.Id })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Client.Id > list[j].Client.Id })
		}
	} else if sortField == "Remark" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Remark < list[j].Remark })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Remark > list[j].Remark })
		}
	} else if sortField == "Client.VerifyKey" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Client.VerifyKey < list[j].Client.VerifyKey })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Client.VerifyKey > list[j].Client.VerifyKey })
		}
	} else if sortField == "Host" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Host < list[j].Host })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Host > list[j].Host })
		}
	} else if sortField == "Scheme" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Scheme < list[j].Scheme })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Scheme > list[j].Scheme })
		}
	} else if sortField == "Target.TargetStr" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Target.TargetStr < list[j].Target.TargetStr })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Target.TargetStr > list[j].Target.TargetStr })
		}
	} else if sortField == "Location" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Location < list[j].Location })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Location > list[j].Location })
		}
	} else if sortField == "IsClose" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].IsClose && !list[j].IsClose })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return !list[i].IsClose && list[j].IsClose })
		}
	} else if sortField == "Client.IsConnect" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Client.IsConnect && !list[j].Client.IsConnect })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return !list[i].Client.IsConnect && list[j].Client.IsConnect })
		}
	}
	return
}

// get client list
func GetClientList(start, length int, search, sortField, order string, clientId int) (list []*file.Client, cnt int) {
	list, cnt = file.GetDb().GetClientList(start, length, search, sortField, order, clientId)
	//sort by Id, Remark, Port..., asc or desc
	if sortField == "Id" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Id < list[j].Id })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Id > list[j].Id })
		}
	} else if sortField == "Addr" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Addr < list[j].Addr })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Addr > list[j].Addr })
		}
	} else if sortField == "Remark" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Remark < list[j].Remark })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Remark > list[j].Remark })
		}
	} else if sortField == "VerifyKey" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].VerifyKey < list[j].VerifyKey })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].VerifyKey > list[j].VerifyKey })
		}
	} else if sortField == "Version" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Version < list[j].Version })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Version > list[j].Version })
		}
	} else if sortField == "Rate.NowRate" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Rate.NowRate < list[j].Rate.NowRate })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Rate.NowRate > list[j].Rate.NowRate })
		}
	} else if sortField == "Status" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].Status && !list[j].Status })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return !list[i].Status && list[j].Status })
		}
	} else if sortField == "IsConnect" {
		if order == "asc" {
			sort.SliceStable(list, func(i, j int) bool { return list[i].IsConnect && !list[j].IsConnect })
		} else {
			sort.SliceStable(list, func(i, j int) bool { return !list[i].IsConnect && list[j].IsConnect })
		}
	}
	dealClientData()
	return
}

func dealClientData() {
	//logs.Info("dealClientData.........")

	file.GetDb().JsonDb.Clients.Range(func(key, value interface{}) bool {
		v := value.(*file.Client)
		if vv, ok := Bridge.Client.Load(v.Id); ok {
			v.IsConnect = true
			v.LastOnlineTime = time.Now().Format("2006-01-02 15:04:05")
			v.Version = vv.(*bridge.Client).Version
		} else if v.Id <= 0 {
			if allowLocalProxy, _ := beego.AppConfig.Bool("allow_local_proxy"); allowLocalProxy {
				v.IsConnect = true
				v.Version = version.VERSION
				// 如果客户端 ID 小于等于 0 且允许本地代理，插入虚拟客户端
				if _, exists := Bridge.Client.Load(v.Id); !exists {
					// 创建虚拟客户端并插入
					Bridge.Client.LoadOrStore(v.Id, bridge.NewClient(nil, nil, nil, version.VERSION))
					logs.Debug("Inserted virtual client for ID %d", v.Id)
				}
			} else {
				v.IsConnect = false
			}
		} else {
			v.IsConnect = false
		}
		//v.Flow.InletFlow = 0
		//v.Flow.ExportFlow = 0
		//if len(file.GetDb().JsonDb.Hosts) == 0 {
		//
		//}
		//var inflow int64 = 0
		//var outflow int64 = 0
		//file.GetDb().JsonDb.Hosts.Range(func(key, value interface{}) bool {
		//	h := value.(*file.Host)
		//	if h.Client.Id == v.Id {
		//		inflow  += h.Flow.InletFlow
		//		outflow += h.Flow.ExportFlow
		//	}
		//	return true
		//})
		//file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
		//	t := value.(*file.Tunnel)
		//	if t.Client.Id == v.Id {
		//		inflow  += t.Flow.InletFlow
		//		outflow += t.Flow.ExportFlow
		//	}
		//	return true
		//})
		//
		//if inflow >0 || outflow >0{
		//	v.Flow.InletFlow = inflow
		//	v.Flow.ExportFlow = outflow
		//}
		return true
	})
	return
}

// delete all host and tasks by client id
func DelTunnelAndHostByClientId(clientId int, justDelNoStore bool) {
	var ids []int
	file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
		v := value.(*file.Tunnel)
		if justDelNoStore && !v.NoStore {
			return true
		}
		if v.Client.Id == clientId {
			ids = append(ids, v.Id)
		}
		return true
	})
	for _, id := range ids {
		DelTask(id)
	}
	ids = ids[:0]
	file.GetDb().JsonDb.Hosts.Range(func(key, value interface{}) bool {
		v := value.(*file.Host)
		if justDelNoStore && !v.NoStore {
			return true
		}
		if v.Client.Id == clientId {
			ids = append(ids, v.Id)
		}
		return true
	})
	for _, id := range ids {
		file.GetDb().DelHost(id)
	}
	UpdateProxyPoolFile()
}
func DelClientConnect(clientId int) {
	Bridge.DelClient(clientId)
}

func GetDashboardData() map[string]interface{} {
	data := make(map[string]interface{})
	data["version"] = version.VERSION
	data["hostCount"] = common.GeSynctMapLen(file.GetDb().JsonDb.Hosts)
	data["clientCount"] = common.GeSynctMapLen(file.GetDb().JsonDb.Clients)
	if beego.AppConfig.String("public_vkey") != "" { //remove public vkey
		data["clientCount"] = data["clientCount"].(int) - 1
	}
	dealClientData()
	c := 0
	var in, out int64
	file.GetDb().JsonDb.Clients.Range(func(key, value interface{}) bool {
		v := value.(*file.Client)
		if v.IsConnect {
			c += 1
		}
		in += v.Flow.InletFlow
		out += v.Flow.ExportFlow
		return true
	})
	data["clientOnlineCount"] = c
	data["inletFlowCount"] = int(in)
	data["exportFlowCount"] = int(out)
	var tcp, udp, secret, socks5, p2p, http int
	file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
		switch value.(*file.Tunnel).Mode {
		case "tcp":
			tcp += 1
		case "socks5":
			socks5 += 1
		case "httpProxy":
			http += 1
		case "udp":
			udp += 1
		case "p2p":
			p2p += 1
		case "secret":
			secret += 1
		}
		return true
	})

	data["tcpC"] = tcp
	data["udpCount"] = udp
	data["socks5Count"] = socks5
	data["httpProxyCount"] = http
	data["secretCount"] = secret
	data["p2pCount"] = p2p
	bridgeType := beego.AppConfig.String("bridge_type")
	if bridgeType == "both" {
		bridgeType = "tcp"
	}
	data["bridgeType"] = bridgeType
	data["httpProxyPort"] = beego.AppConfig.String("http_proxy_port")
	data["httpsProxyPort"] = beego.AppConfig.String("https_proxy_port")
	data["ipLimit"] = beego.AppConfig.String("ip_limit")
	data["flowStoreInterval"] = beego.AppConfig.String("flow_store_interval")
	//data["serverIp"] = beego.AppConfig.String("p2p_ip")
	data["serverIp"] = common.GetServerIp()
	data["p2pPort"] = beego.AppConfig.String("p2p_port")
	data["logLevel"] = beego.AppConfig.String("log_level")
	tcpCount := 0

	file.GetDb().JsonDb.Clients.Range(func(key, value interface{}) bool {
		tcpCount += int(value.(*file.Client).NowConn)
		return true
	})
	data["tcpCount"] = tcpCount
	cpuPercet, _ := cpu.Percent(0, true)
	var cpuAll float64
	for _, v := range cpuPercet {
		cpuAll += v
	}
	loads, _ := load.Avg()
	data["load"] = loads.String()
	data["cpu"] = math.Round(cpuAll / float64(len(cpuPercet)))
	swap, _ := mem.SwapMemory()
	data["swap_mem"] = math.Round(swap.UsedPercent)
	vir, _ := mem.VirtualMemory()
	data["virtual_mem"] = math.Round(vir.UsedPercent)
	conn, _ := net.ProtoCounters(nil)
	io1, _ := net.IOCounters(false)
	time.Sleep(time.Millisecond * 500)
	io2, _ := net.IOCounters(false)
	if len(io2) > 0 && len(io1) > 0 {
		data["io_send"] = (io2[0].BytesSent - io1[0].BytesSent) * 2
		data["io_recv"] = (io2[0].BytesRecv - io1[0].BytesRecv) * 2
	}
	for _, v := range conn {
		data[v.Protocol] = v.Stats["CurrEstab"]
	}
	//chart
	var fg int
	if len(tool.ServerStatus) >= 10 {
		fg = len(tool.ServerStatus) / 10
		for i := 0; i <= 9; i++ {
			data["sys"+strconv.Itoa(i+1)] = tool.ServerStatus[i*fg]
		}
	}
	return data
}

// 获取服务端版本号
func GetVersion() string {
	return version.VERSION
}

// 获取年份
func GetCurrentYear() int {
	return time.Now().Year()
}

// 实例化流量数据到文件
func flowSession(m time.Duration) {
	file.GetDb().JsonDb.StoreHostToJsonFile()
	file.GetDb().JsonDb.StoreTasksToJsonFile()
	file.GetDb().JsonDb.StoreClientsToJsonFile()
	file.GetDb().JsonDb.StoreGlobalToJsonFile()
	once.Do(func() {
		ticker := time.NewTicker(m)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				file.GetDb().JsonDb.StoreHostToJsonFile()
				file.GetDb().JsonDb.StoreTasksToJsonFile()
				file.GetDb().JsonDb.StoreClientsToJsonFile()
				file.GetDb().JsonDb.StoreGlobalToJsonFile()
			}
		}
	})
}

// AutoCreateSocks5ForClient creates an ephemeral SOCKS5 tunnel for the given client
func AutoCreateSocks5ForClient(clientId int) {
	// Check if feature is enabled
	if enabled, _ := beego.AppConfig.Bool("socks5_proxy_pool_enable"); !enabled {
		return
	}

	// Skip virtual clients
	if clientId <= 0 {
		return
	}

	// Get client info
	client, err := file.GetDb().GetClient(clientId)
	if err != nil {
		logs.Warn("AutoCreateSocks5: client %d not found: %v", clientId, err)
		return
	}

	// Check if client already has a SOCKS5 tunnel
	hasSocks5 := false
	file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
		v := value.(*file.Tunnel)
		if v.Client.Id == clientId && v.Mode == "socks5" {
			hasSocks5 = true
			return false
		}
		return true
	})
	if hasSocks5 {
		logs.Debug("AutoCreateSocks5: client %d already has SOCKS5 tunnel, skipping", clientId)
		return
	}

	// Find next available port starting from configured base
	portBase, _ := beego.AppConfig.Int("socks5_proxy_pool_port_base")
	if portBase == 0 {
		portBase = 10800
	}

	// Collect all used ports to avoid conflicts
	usedPorts := make(map[int]bool)
	file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
		v := value.(*file.Tunnel)
		if v.Port > 0 {
			usedPorts[v.Port] = true
		}
		return true
	})

	var port int
	for p := portBase; p <= 65535; p++ {
		if !usedPorts[p] && tool.TestServerPort(p, "socks5") {
			port = p
			break
		}
	}
	if port == 0 {
		logs.Error("AutoCreateSocks5: no available port for client %d", clientId)
		return
	}

	// Generate random credentials to prevent port scanning abuse
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	genRandom := func(length int) string {
		b := make([]byte, length)
		for i := range b {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
			b[i] = charset[n.Int64()]
		}
		return string(b)
	}
	randomUser := "u" + genRandom(11)
	randomPass := genRandom(16)
	authContent := randomUser + ":" + randomPass

	// Also set Client-level Basic auth so NPS web panel displays it
	client.Cnf.U = randomUser
	client.Cnf.P = randomPass
	file.GetDb().JsonDb.Clients.Store(client.Id, client)

	// Create the tunnel
	t := &file.Tunnel{
		Mode:     "socks5",
		Port:     port,
		ServerIp: "0.0.0.0",
		Client:   client,
		Id:       int(file.GetDb().JsonDb.GetTaskId()),
		Status:   true,
		Flow:     new(file.Flow),
		NoStore:  true,
		Remark:   fmt.Sprintf("auto-socks5-client-%d", clientId),
		Target:   &file.Target{},
		UserAuth: &file.MultiAccount{
			Content:    authContent,
			AccountMap: map[string]string{randomUser: randomPass},
		},
	}

	if err := file.GetDb().NewTask(t); err != nil {
		logs.Error("AutoCreateSocks5: failed to create task for client %d: %v", clientId, err)
		return
	}

	if err := AddTask(t); err != nil {
		logs.Error("AutoCreateSocks5: failed to start tunnel for client %d: %v", clientId, err)
		file.GetDb().DelTask(t.Id)
		return
	}

	logs.Info("AutoCreateSocks5: created SOCKS5 tunnel on port %d for client %d", port, clientId)
	UpdateProxyPoolFile()
}
