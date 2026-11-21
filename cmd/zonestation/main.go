// zonestation 是 DNS 区数据采集站：单个进程内同时运行
// 本地权威测试 DNS 服务、采集引擎与网页界面。
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zonestation/internal/collector"
	"zonestation/internal/httpapi"
	"zonestation/internal/station"
	"zonestation/internal/testdns"
)

func main() {
	httpAddr := flag.String("http", envOr("ZONESTATION_HTTP", ":8080"), "网页与 API 监听地址")
	dnsAddr := flag.String("dns", envOr("ZONESTATION_DNS", "127.0.0.1:5353"), "测试 DNS 服务监听地址")
	flag.Parse()

	ts, err := testdns.New(*dnsAddr)
	if err != nil {
		log.Fatalf("启动测试 DNS 服务失败: %v", err)
	}
	defer ts.Close()

	store := station.New()
	svc := &collector.Service{Addr: ts.Addr(), Timeout: 5 * time.Second}
	api := &httpapi.API{TS: ts, Store: store, Svc: svc}

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("DNS 区数据采集站已启动")
	log.Printf("  网页与 API:  %s", displayHTTP(*httpAddr))
	log.Printf("  测试 DNS:    %s (TCP/UDP)，区: %v", ts.Addr(), ts.ZoneNames())
	log.Printf("  试用:        dig @%s -p %s example.com. SOA", hostOf(ts.Addr()), portOf(ts.Addr()))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Printf("收到信号 %v，正在退出", s)
		_ = srv.Close()
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常: %v", err)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func displayHTTP(addr string) string {
	if addr == "" {
		return "http://localhost:8080"
	}
	if addr[0] == ':' {
		return "http://localhost" + addr
	}
	return "http://" + addr
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return p
}
