package core

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// errStopAfterHello 让测试服务器收到 ClientHello 后立刻中断握手。
// 本用例只关心客户端提议了什么 ALPN，不需要真完成 TLS 会话。
var errStopAfterHello = errors.New("test: stop after client hello")

// captureALPN 起一个本地 TLS 服务器，抓取首个 ClientHello 里的 ALPN 提议。
// 返回的 addr 供被测拨号函数连接，protos 通道传出提议列表。
func captureALPN(t *testing.T) (addr string, protos <-chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ch := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		srv := tls.Server(c, &tls.Config{
			GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
				select {
				case ch <- chi.SupportedProtos:
				default:
				}
				return nil, errStopAfterHello
			},
		})
		_ = srv.Handshake() // 预期失败：抓完 ClientHello 就中断
	}()
	return ln.Addr().String(), ch
}

// TestNewUTLSConnForcesHTTP11ALPN 固定「握手必须只提议 http/1.1」这条不变量。
//
// 背景（实测）：HelloChrome_Auto 的 ALPN 是 ["h2","http/1.1"]，B 站 CDN 会
// 选中 h2；而 Transport 自定义了 DialTLSContext 后 Go 不做 HTTP/2，请求仍按
// HTTP/1.1 发出，服务器回的 HTTP/2 帧让 Transport 抛
// "malformed HTTP response" 加一串二进制，真实原因（如 404 签名过期）被盖住。
func TestNewUTLSConnForcesHTTP11ALPN(t *testing.T) {
	addr, protos := captureALPN(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()

	uConn, err := newUTLSConn(conn, "localhost")
	if err != nil {
		t.Fatalf("newUTLSConn: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = uConn.HandshakeContext(ctx) // 服务器会中断，握手错误不影响断言

	assertOnlyHTTP11(t, protos)
}

// TestDialDirectForcesHTTP11ALPN 覆盖直连路径本身——缺陷正是「直连那份实现
// 漏了 ALPN 收窄」。只测 newUTLSConn 抓不住它：函数正确但没人调用照样回归。
func TestDialDirectForcesHTTP11ALPN(t *testing.T) {
	addr, protos := captureALPN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = dialDirect(ctx, addr) // 握手预期失败，只看 ClientHello

	assertOnlyHTTP11(t, protos)
}

// assertOnlyHTTP11 断言客户端只提议 http/1.1。
func assertOnlyHTTP11(t *testing.T, protos <-chan []string) {
	t.Helper()
	select {
	case got := <-protos:
		if len(got) != 1 || got[0] != "http/1.1" {
			t.Fatalf("ALPN 提议 = %v，必须恰好是 [http/1.1]；"+
				"提议 h2 会让支持 h2 的 CDN 返回 HTTP/2 帧，客户端只能报 malformed HTTP response", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("本地服务器未收到 ClientHello（拨号路径可能没连上）")
	}
}
