package common

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

var customDnsAddr string

func SetCustomDNS(dnsAddr string) {
	if dnsAddr == "" {
		return
	}
	colonCount := strings.Count(dnsAddr, ":")
	if colonCount == 0 {
		dnsAddr += ":53"
	} else if colonCount > 1 && !strings.Contains(dnsAddr, "]:") {
		if strings.Contains(dnsAddr, "]") {
			dnsAddr += ":53"
		} else {
			dnsAddr = "[" + dnsAddr + "]:53"
		}
	}

	customDnsAddr = dnsAddr

	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return net.Dial(network, dnsAddr)
		},
	}
}

func GetCustomDNS() string {
	if customDnsAddr != "" {
		return customDnsAddr
	}
	return "8.8.8.8:53"
}

func GetFastAddr(addr string, testType string) (string, error) {
	host := GetIpByAddr(addr)

	ip := net.ParseIP(host)
	if ip != nil {
		return addr, nil
	}

	port := GetPortByAddr(addr)
	ipv4List, ipv6List, err := resolveDomain(host)
	if err != nil {
		return addr, err
	}

	if len(ipv4List) == 0 && len(ipv6List) == 0 {
		return addr, fmt.Errorf("can not resolve %s", host)
	}

	ipList := append(ipv4List, ipv6List...)
	ipList = unique(ipList)

	bestIP, bestLatency := ipList[0], time.Duration(1<<63-1)
	for _, ip := range ipList {
		latency, err := TestLatency(BuildAddress(ip, strconv.Itoa(port)), testType)
		if err != nil {
			continue
		}
		if latency < bestLatency {
			bestIP, bestLatency = ip, latency
		}
	}
	//logs.Debug("Final Best IP: %s", bestIP)
	if bestLatency == time.Duration(1<<63-1) {
		return addr, nil
	}
	return BuildAddress(bestIP, strconv.Itoa(port)), nil
}

func resolveDomain(domain string) (ipv4List, ipv6List []string, err error) {
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", domain)
	if err != nil {
		return nil, nil, err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			ipv4List = append(ipv4List, v4.String())
			continue
		}
		ipv6List = append(ipv6List, ip.String())
	}
	if len(ipv4List) == 0 && len(ipv6List) == 0 {
		return nil, nil, fmt.Errorf("can not resolve %s", domain)
	}
	return ipv4List, ipv6List, nil
}

func TestLatency(addr string, testType string) (time.Duration, error) {
	start := time.Now()
	switch testType {
	case "tcp", "tls":
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return 0, err
		}
		defer func() { _ = conn.Close() }()
	default:
		return 0, fmt.Errorf("unsupported test type: %s", testType)
	}
	return time.Since(start), nil
}

func unique(addrs []string) []string {
	seen := make(map[string]struct{}, len(addrs))
	var out []string
	for _, a := range addrs {
		if _, ok := seen[a]; !ok {
			seen[a] = struct{}{}
			out = append(out, a)
		}
	}
	return out
}
