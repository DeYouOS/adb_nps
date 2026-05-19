package bridge

import (
	"crypto/tls"
	_ "crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beego/beego"
	"github.com/djylb/nps/lib/common"
	"github.com/djylb/nps/lib/conn"
	"github.com/djylb/nps/lib/crypt"
	"github.com/djylb/nps/lib/file"
	"github.com/djylb/nps/lib/logs"
	"github.com/djylb/nps/lib/nps_mux"
	"github.com/djylb/nps/lib/version"
	"github.com/djylb/nps/server/connection"
	"github.com/djylb/nps/server/tool"
)

var ServerTlsEnable bool = false
var ServerKcpEnable bool = false

type Client struct {
	signals   []*conn.Conn     // WORK_MAIN connections (control signals)
	tunnels   []*nps_mux.Mux   // WORK_CHAN connections (data tunnels)
	files     []*nps_mux.Mux   // WORK_FILE connections (file transfer)
	mu        sync.RWMutex     // protects all slices
	Version   string
	retryTime int              // it will be add 1 when ping not ok until to 3 will close the client
	rrIndex   uint64           // round-robin counter for tunnel selection
}

func NewClient(t, f *nps_mux.Mux, s *conn.Conn, vs string) *Client {
	client := &Client{
		signals: make([]*conn.Conn, 0),
		tunnels: make([]*nps_mux.Mux, 0),
		files:   make([]*nps_mux.Mux, 0),
		Version: vs,
	}
	if s != nil {
		client.signals = append(client.signals, s)
	}
	if t != nil {
		client.tunnels = append(client.tunnels, t)
	}
	if f != nil {
		client.files = append(client.files, f)
	}
	return client
}

type Bridge struct {
	TunnelPort      int //通信隧道端口
	Client          *sync.Map
	Register        *sync.Map
	tunnelType      string //bridge type kcp or tcp
	OpenTask        chan *file.Tunnel
	CloseTask       chan *file.Tunnel
	CloseClient     chan int
	SecretChan      chan *conn.Secret
	ipVerify        bool
	runList         *sync.Map //map[int]interface{}
	disconnectTime  int
	OnClientConnect func(id int) // callback for auto SOCKS5 tunnel creation
}

func NewTunnel(tunnelPort int, tunnelType string, ipVerify bool, runList *sync.Map, disconnectTime int) *Bridge {
	return &Bridge{
		TunnelPort:     tunnelPort,
		tunnelType:     tunnelType,
		Client:         &sync.Map{},
		Register:       &sync.Map{},
		OpenTask:       make(chan *file.Tunnel, 100),
		CloseTask:      make(chan *file.Tunnel, 100),
		CloseClient:    make(chan int, 100),
		SecretChan:     make(chan *conn.Secret, 100),
		ipVerify:       ipVerify,
		runList:        runList,
		disconnectTime: disconnectTime,
	}
}

func (s *Bridge) StartTunnel() error {
	go s.ping()
	if s.tunnelType == "kcp" {
		logs.Info("server start, the bridge type is %s, the bridge port is %d", s.tunnelType, s.TunnelPort)
		return conn.NewKcpListenerAndProcess(common.BuildAddress(beego.AppConfig.String("bridge_ip"), beego.AppConfig.String("bridge_port")), func(c net.Conn) {
			s.cliProcess(conn.NewConn(c))
		})
	} else {
		// tcp
		go func() {
			listener, err := connection.GetBridgeTcpListener()
			if err != nil {
				logs.Error("%v", err)
				os.Exit(0)
				return
			}
			conn.Accept(listener, func(c net.Conn) {
				s.cliProcess(conn.NewConn(c))
			})
		}()

		// tls
		if ServerTlsEnable {
			go func() {
				tlsListener, tlsErr := connection.GetBridgeTlsListener()
				if tlsErr != nil {
					logs.Error("%v", tlsErr)
					os.Exit(0)
					return
				}
				conn.Accept(tlsListener, func(c net.Conn) {
					s.cliProcess(conn.NewConn(tls.Server(c, &tls.Config{Certificates: []tls.Certificate{crypt.GetCert()}})))
				})
			}()
		}

		// kcp
		if ServerKcpEnable {
			go func() {
				bridgeKcp := *s
				bridgeKcp.tunnelType = "kcp"
				conn.NewKcpListenerAndProcess(common.BuildAddress(beego.AppConfig.String("bridge_ip"), beego.AppConfig.String("bridge_port")), func(c net.Conn) {
					bridgeKcp.cliProcess(conn.NewConn(c))
				})
			}()
		}
	}
	return nil
}

// get health information form client
func (s *Bridge) GetHealthFromClient(id int, c *conn.Conn) {
	// 跳过虚拟客户端
	if id <= 0 {
		return
	}

	for {
		if info, status, err := c.GetHealthInfo(); err != nil {
			break
		} else if !status { //the status is true , return target to the targetArr
			file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
				v := value.(*file.Tunnel)
				if v.Client.Id == id && v.Mode == "tcp" && strings.Contains(v.Target.TargetStr, info) {
					v.Lock()
					if v.Target.TargetArr == nil || (len(v.Target.TargetArr) == 0 && len(v.HealthRemoveArr) == 0) {
						v.Target.TargetArr = common.TrimArr(strings.Split(v.Target.TargetStr, "\n"))
					}
					v.Target.TargetArr = common.RemoveArrVal(v.Target.TargetArr, info)
					if v.HealthRemoveArr == nil {
						v.HealthRemoveArr = make([]string, 0)
					}
					v.HealthRemoveArr = append(v.HealthRemoveArr, info)
					v.Unlock()
				}
				return true
			})
			file.GetDb().JsonDb.Hosts.Range(func(key, value interface{}) bool {
				v := value.(*file.Host)
				if v.Client.Id == id && strings.Contains(v.Target.TargetStr, info) {
					v.Lock()
					if v.Target.TargetArr == nil || (len(v.Target.TargetArr) == 0 && len(v.HealthRemoveArr) == 0) {
						v.Target.TargetArr = common.TrimArr(strings.Split(v.Target.TargetStr, "\n"))
					}
					v.Target.TargetArr = common.RemoveArrVal(v.Target.TargetArr, info)
					if v.HealthRemoveArr == nil {
						v.HealthRemoveArr = make([]string, 0)
					}
					v.HealthRemoveArr = append(v.HealthRemoveArr, info)
					v.Unlock()
				}
				return true
			})
		} else { //the status is false,remove target from the targetArr
			file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
				v := value.(*file.Tunnel)
				if v.Client.Id == id && v.Mode == "tcp" && common.IsArrContains(v.HealthRemoveArr, info) && !common.IsArrContains(v.Target.TargetArr, info) {
					v.Lock()
					v.Target.TargetArr = append(v.Target.TargetArr, info)
					v.HealthRemoveArr = common.RemoveArrVal(v.HealthRemoveArr, info)
					v.Unlock()
				}
				return true
			})

			file.GetDb().JsonDb.Hosts.Range(func(key, value interface{}) bool {
				v := value.(*file.Host)
				if v.Client.Id == id && common.IsArrContains(v.HealthRemoveArr, info) && !common.IsArrContains(v.Target.TargetArr, info) {
					v.Lock()
					v.Target.TargetArr = append(v.Target.TargetArr, info)
					v.HealthRemoveArr = common.RemoveArrVal(v.HealthRemoveArr, info)
					v.Unlock()
				}
				return true
			})
		}
	}
	// This specific signal connection died. Remove it from the pool.
	// Only fully delete the client if no connections remain.
	if v, ok := s.Client.Load(id); ok {
		client := v.(*Client)
		client.mu.Lock()
		for i, sig := range client.signals {
			if sig == c {
				client.signals = append(client.signals[:i], client.signals[i+1:]...)
				break
			}
		}
		hasConnections := len(client.signals) > 0 || len(client.tunnels) > 0
		client.mu.Unlock()
		if !hasConnections {
			s.DelClient(id)
		}
	}
}

// 验证失败，返回错误验证flag，并且关闭连接
func (s *Bridge) verifyError(c *conn.Conn) {
	c.Write([]byte(common.VERIFY_EER))
	c.Close()
}

func (s *Bridge) verifySuccess(c *conn.Conn) {
	c.Write([]byte(common.VERIFY_SUCCESS))
}

func (s *Bridge) cliProcess(c *conn.Conn) {
	if c.Conn == nil || c.Conn.RemoteAddr() == nil {
		logs.Warn("Invalid connection")
		return
	}

	//read test flag
	if _, err := c.GetShortContent(3); err != nil {
		logs.Info("The client %v connect error: %v", c.Conn.RemoteAddr(), err)
		c.Close()
		return
	}
	//version check
	if ver, err := c.GetShortLenContent(); err != nil || string(ver) != version.GetVersion() {
		logs.Info("The client %v version does not match or error occurred", c.Conn.RemoteAddr())
		//c.Close()
		//common.SafeClose(c)
		//return
	}
	//version get
	var vs []byte
	var err error
	if vs, err = c.GetShortLenContent(); err != nil {
		logs.Info("Get client %v version error: %v", c.Conn.RemoteAddr(), err)
		c.Close()
		return
	}
	//write server version to client
	c.Write([]byte(crypt.Md5(version.GetVersion())))
	c.SetReadDeadlineBySecond(5)
	var buf []byte
	//get vKey from client
	if buf, err = c.GetShortContent(32); err != nil {
		c.Close()
		return
	}
	//verify
	id, err := file.GetDb().GetIdByVerifyKey(string(buf), c.Conn.RemoteAddr().String())
	if err != nil {
		logs.Error("Client %v vkey %s validation error, close it's connection.", c.Conn.RemoteAddr(), buf)
		s.verifyError(c)
		return
	} else {
		s.verifySuccess(c)
	}
	if flag, err := c.ReadFlag(); err == nil {
		s.typeDeal(flag, c, id, string(vs))
	} else {
		logs.Warn("%v %s", err, flag)
	}
	return
}

func (s *Bridge) DelClient(id int) {
	if v, ok := s.Client.Load(id); ok {
		client := v.(*Client)
		client.mu.Lock()

		for _, sig := range client.signals {
			if sig != nil {
				sig.Close()
			}
		}
		for _, t := range client.tunnels {
			if t != nil {
				t.Close()
			}
		}
		for _, f := range client.files {
			if f != nil {
				f.Close()
			}
		}
		client.signals = nil
		client.tunnels = nil
		client.files = nil

		client.mu.Unlock()
		s.Client.Delete(id)

		if file.GetDb().IsPubClient(id) {
			return
		}
		if c, err := file.GetDb().GetClient(id); err == nil {
			select {
			case s.CloseClient <- c.Id:
			default:
				logs.Warn("CloseClient channel is full, failed to send close signal for client %d", c.Id)
			}
		}
	}
}

// use different
func (s *Bridge) typeDeal(typeVal string, c *conn.Conn, id int, vs string) {
	isPub := file.GetDb().IsPubClient(id)
	switch typeVal {
	case common.WORK_MAIN:
		if isPub {
			c.Close()
			return
		}
		tcpConn, ok := c.Conn.(*net.TCPConn)
		if ok {
			// add tcp keep alive option for signal connection
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(5 * time.Second)
		}

		// Append new signal connection (support multiple devices with same vkey)
		if v, loaded := s.Client.LoadOrStore(id, NewClient(nil, nil, c, vs)); loaded {
			client := v.(*Client)
			client.mu.Lock()
			client.signals = append(client.signals, c)
			client.Version = vs
			client.mu.Unlock()
		}

		go s.GetHealthFromClient(id, c)
		logs.Info("clientId %d connection succeeded, address:%v ", id, c.Conn.RemoteAddr())

		// Trigger auto SOCKS5 tunnel creation callback
		if s.OnClientConnect != nil {
			go s.OnClientConnect(id)
		}

	case common.WORK_CHAN:
		muxConn := nps_mux.NewMux(c.Conn, s.tunnelType, s.disconnectTime)
		if v, loaded := s.Client.LoadOrStore(id, NewClient(muxConn, nil, nil, vs)); loaded {
			client := v.(*Client)
			client.mu.Lock()
			client.tunnels = append(client.tunnels, muxConn)
			client.mu.Unlock()
		}

	case common.WORK_CONFIG:
		client, err := file.GetDb().GetClient(id)
		if err != nil || (!isPub && !client.ConfigConnAllow) {
			c.Close()
			return
		}
		binary.Write(c, binary.LittleEndian, isPub)
		go s.getConfig(c, isPub, client)

	case common.WORK_REGISTER:
		go s.register(c)

	case common.WORK_SECRET:
		if b, err := c.GetShortContent(32); err == nil {
			s.SecretChan <- conn.NewSecret(string(b), c)
		} else {
			logs.Error("secret error, failed to match the key successfully")
		}

	case common.WORK_FILE:
		muxConn := nps_mux.NewMux(c.Conn, s.tunnelType, s.disconnectTime)
		if v, loaded := s.Client.LoadOrStore(id, NewClient(nil, muxConn, nil, vs)); loaded {
			client := v.(*Client)
			client.mu.Lock()
			client.files = append(client.files, muxConn)
			client.mu.Unlock()
		}

	case common.WORK_P2P:
		// read md5 secret
		if b, err := c.GetShortContent(32); err != nil {
			logs.Error("p2p error, %v", err)
		} else if t := file.GetDb().GetTaskByMd5Password(string(b)); t == nil {
			logs.Error("p2p error, failed to match the key successfully")
		} else if v, ok := s.Client.Load(t.Client.Id); ok {
			//向密钥对应的客户端发送与服务端udp建立连接信息，地址，密钥
			serverIP := common.GetServerIp()
			serverPort := beego.AppConfig.String("p2p_port")

			svrAddr := common.BuildAddress(serverIP, serverPort)
			if serverPort == "" {
				logs.Warn("get local udp addr error")
				return
			}
			client := v.(*Client)
			client.mu.RLock()
			if len(client.signals) == 0 {
				client.mu.RUnlock()
				return
			}
			sig := client.signals[0]
			client.mu.RUnlock()
			sig.Write([]byte(common.NEW_UDP_CONN))
			sig.WriteLenContent([]byte(svrAddr))
			sig.WriteLenContent(b)
			//向该请求者发送建立连接请求,服务器地址
			c.WriteLenContent([]byte(svrAddr))

		} else {
			return
		}
	}

	c.SetAlive() // 设置连接为活动状态，避免超时断开
	return
}

// register ip
func (s *Bridge) register(c *conn.Conn) {
	var hour int32
	if err := binary.Read(c, binary.LittleEndian, &hour); err == nil {
		ip := common.GetIpByAddr(c.Conn.RemoteAddr().String())
		s.Register.Store(ip, time.Now().Add(time.Hour*time.Duration(hour)))
		logs.Info("Registered IP: %s for %d hours", ip, hour)
	} else {
		logs.Warn("Failed to register IP: %v", err)
	}
}

func (s *Bridge) SendLinkInfo(clientId int, link *conn.Link, t *file.Tunnel) (target net.Conn, err error) {
	// if the proxy type is local
	if link.LocalProxy {
		target, err = net.Dial("tcp", link.Host)
		return
	}

	clientValue, ok := s.Client.Load(clientId)
	if !ok {
		err = errors.New(fmt.Sprintf("the client %d is not connect", clientId))
		return
	}

	client := clientValue.(*Client)
	// If IP is restricted, do IP verification
	if s.ipVerify {
		ip := common.GetIpByAddr(link.RemoteAddr)
		ipValue, ok := s.Register.Load(ip)
		if !ok {
			return nil, errors.New(fmt.Sprintf("The ip %s is not in the validation list", ip))
		}

		if !ipValue.(time.Time).After(time.Now()) {
			return nil, errors.New(fmt.Sprintf("The validity of the ip %s has expired", ip))
		}
	}

	var tunnel *nps_mux.Mux
	if t != nil && t.Mode == "file" {
		// Pick first available file connection
		client.mu.RLock()
		for _, f := range client.files {
			if f != nil && !f.IsClose {
				tunnel = f
				break
			}
		}
		client.mu.RUnlock()
	} else {
		// Round-robin across available tunnel connections
		client.mu.RLock()
		if len(client.tunnels) > 0 {
			n := len(client.tunnels)
			start := atomic.AddUint64(&client.rrIndex, 1) % uint64(n)
			for i := 0; i < n; i++ {
				idx := (int(start) + i) % n
				if client.tunnels[idx] != nil && !client.tunnels[idx].IsClose {
					tunnel = client.tunnels[idx]
					break
				}
			}
		}
		client.mu.RUnlock()
	}

	if tunnel == nil {
		err = errors.New("the client connect error")
		return
	}

	target, err = tunnel.NewConn()
	if err != nil {
		return
	}

	if t != nil && t.Mode == "file" {
		//TODO if t.mode is file ,not use crypt or compress
		link.Crypt = false
		link.Compress = false
		return
	}

	if _, err = conn.NewConn(target).SendInfo(link, ""); err != nil {
		logs.Info("new connection error, the target %s refused to connect", link.Host)
		return
	}

	return
}

func (s *Bridge) ping() {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			closedClients := make([]int, 0)

			s.Client.Range(func(key, value interface{}) bool {
				clientID := key.(int)
				client := value.(*Client)

				// 跳过虚拟客户端的健康检查
				if clientID <= 0 {
					return true
				}

				// Check if ALL connections are dead
				client.mu.RLock()
				hasHealthyTunnel := false
				for _, t := range client.tunnels {
					if t != nil && !t.IsClose {
						hasHealthyTunnel = true
						break
					}
				}
				hasHealthySignal := len(client.signals) > 0
				client.mu.RUnlock()

				if client == nil || (!hasHealthyTunnel && !hasHealthySignal) {
					client.retryTime++
					if client.retryTime >= 3 {
						closedClients = append(closedClients, clientID)
					}
				} else {
					client.retryTime = 0 // Reset retry count when the state is normal
				}
				return true
			})

			for _, clientId := range closedClients {
				logs.Info("the client %d closed", clientId)
				s.DelClient(clientId)
			}
		}
	}
}

// get config and add task from client config
func (s *Bridge) getConfig(c *conn.Conn, isPub bool, client *file.Client) {
	var fail bool
loop:
	for {
		flag, err := c.ReadFlag()
		if err != nil {
			break
		}

		switch flag {
		case common.WORK_STATUS:
			b, err := c.GetShortContent(32)
			if err != nil {
				break loop
			}

			id, err := file.GetDb().GetClientIdByVkey(string(b))
			if err != nil {
				break loop
			}

			var strBuilder strings.Builder
			file.GetDb().JsonDb.Hosts.Range(func(key, value interface{}) bool {
				v := value.(*file.Host)
				if v.Client.Id == id {
					strBuilder.WriteString(v.Remark + common.CONN_DATA_SEQ)
				}
				return true
			})

			file.GetDb().JsonDb.Tasks.Range(func(key, value interface{}) bool {
				v := value.(*file.Tunnel)
				if _, ok := s.runList.Load(v.Id); ok && v.Client.Id == id {
					strBuilder.WriteString(v.Remark + common.CONN_DATA_SEQ)
				}
				return true
			})

			str := strBuilder.String()
			binary.Write(c, binary.LittleEndian, int32(len([]byte(str))))
			binary.Write(c, binary.LittleEndian, []byte(str))

		case common.NEW_CONF:
			client, err = c.GetConfigInfo()
			if err != nil {
				fail = true
				c.WriteAddFail()
				break loop
			}

			if err = file.GetDb().NewClient(client); err != nil {
				fail = true
				c.WriteAddFail()
				break loop
			}

			c.WriteAddOk()
			c.Write([]byte(client.VerifyKey))
			s.Client.LoadOrStore(client.Id, NewClient(nil, nil, nil, ""))

		case common.NEW_HOST:
			h, err := c.GetHostInfo()
			if err != nil {
				fail = true
				c.WriteAddFail()
				break loop
			}

			h.Client = client
			if h.Location == "" {
				h.Location = "/"
			}

			if !client.HasHost(h) {
				if file.GetDb().IsHostExist(h) {
					fail = true
					c.WriteAddFail()
					break loop
				}
				file.GetDb().NewHost(h)
			}
			c.WriteAddOk()

		case common.NEW_TASK:
			t, err := c.GetTaskInfo()
			if err != nil {
				fail = true
				c.WriteAddFail()
				break loop
			}

			ports := common.GetPorts(t.Ports)
			targets := common.GetPorts(t.Target.TargetStr)
			if len(ports) > 1 && (t.Mode == "tcp" || t.Mode == "udp") && (len(ports) != len(targets)) {
				fail = true
				c.WriteAddFail()
				break loop
			} else if t.Mode == "secret" || t.Mode == "p2p" {
				ports = append(ports, 0)
			}

			if len(ports) == 0 {
				fail = true
				c.WriteAddFail()
				break loop
			}

			for i := 0; i < len(ports); i++ {
				tl := &file.Tunnel{
					Mode:         t.Mode,
					Port:         ports[i],
					ServerIp:     t.ServerIp,
					Client:       client,
					Password:     t.Password,
					LocalPath:    t.LocalPath,
					StripPre:     t.StripPre,
					MultiAccount: t.MultiAccount,
					Id:           int(file.GetDb().JsonDb.GetTaskId()),
					Status:       true,
					Flow:         new(file.Flow),
					NoStore:      true,
				}

				if len(ports) == 1 {
					tl.Target = t.Target
					tl.Remark = t.Remark
				} else {
					tl.Remark = fmt.Sprintf("%s_%d", t.Remark, tl.Port)
					if t.TargetAddr != "" {
						tl.Target = &file.Target{
							TargetStr: fmt.Sprintf("%s:%d", t.TargetAddr, targets[i]),
						}
					} else {
						tl.Target = &file.Target{
							TargetStr: strconv.Itoa(targets[i]),
						}
					}
				}

				if !client.HasTunnel(tl) {
					if err := file.GetDb().NewTask(tl); err != nil {
						logs.Warn("add task error: %v", err)
						fail = true
						c.WriteAddFail()
						break loop
					}

					if b := tool.TestServerPort(tl.Port, tl.Mode); !b && t.Mode != "secret" && t.Mode != "p2p" {
						fail = true
						c.WriteAddFail()
						break loop
					}

					s.OpenTask <- tl
				}
				c.WriteAddOk()
			}
		}
	}

	if fail && client != nil {
		s.DelClient(client.Id)
	}
	c.Close()
}
