// 传输层空闲超时：只要还在持续收到数据就不算超时，连续无数据到达才断。
// 回归背景：此前 http.Client.Timeout=90s 覆盖「整个响应体读取」，
// 大文件直链在慢链路上必然被掐断。
package core

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeBody 测试用响应体：按 gap 间隔吐出 chunks。
// hangAtEnd=false 时吐完返回 io.EOF；true 时吐完也不关连接（TCP 半开），
// 只能靠空闲超时把 body 关掉才会返回。
type fakeBody struct {
	chunks    [][]byte
	gap       time.Duration
	hangAtEnd bool
	closed    chan struct{}
	once      sync.Once
}

func newFakeBody(gap time.Duration, hangAtEnd bool, chunks ...string) *fakeBody {
	b := &fakeBody{gap: gap, hangAtEnd: hangAtEnd, closed: make(chan struct{})}
	for _, c := range chunks {
		b.chunks = append(b.chunks, []byte(c))
	}
	return b
}

func (b *fakeBody) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, errors.New("http: read on closed response body")
	case <-time.After(b.gap):
	}
	if len(b.chunks) == 0 {
		if !b.hangAtEnd {
			return 0, io.EOF
		}
		// 服务端停发又不发 FIN：不设上限地等，只能靠空闲超时中断。
		select {
		case <-b.closed:
			return 0, errors.New("http: read on closed response body")
		case <-time.After(time.Hour):
			return 0, io.EOF
		}
	}
	c := b.chunks[0]
	b.chunks = b.chunks[1:]
	return copy(p, c), nil
}

func (b *fakeBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// TestCopyWithIdleTimeoutStopsStalled 卡死（无数据到达）时必须在空闲阈值处中断。
func TestCopyWithIdleTimeoutStopsStalled(t *testing.T) {
	body := newFakeBody(time.Hour, true) // 永远不吐数据
	var out bytes.Buffer
	start := time.Now()
	n, err := copyWithIdleTimeout(&out, body, 80*time.Millisecond)
	if !errors.Is(err, errTransferStalled) {
		t.Fatalf("err=%v want errTransferStalled", err)
	}
	if n != 0 {
		t.Fatalf("n=%d want 0", n)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("中断耗时 %v，空闲超时未生效", elapsed)
	}
}

// TestCopyWithIdleTimeoutToleratesSlowButAlive 慢速但持续有数据到达不得中断
// （这是"固定总时长超时"做错、而"空闲超时"做对的核心区别）。
func TestCopyWithIdleTimeoutToleratesSlowButAlive(t *testing.T) {
	// 5 块数据、每块间隔 30ms：总耗时 150ms > 空闲阈值 80ms，但任意两次
	// 读取间隔都 < 80ms，所以不该被判定为空闲。
	body := newFakeBody(30*time.Millisecond, false, "aaaa", "bbbb", "cccc", "dddd", "eeee")
	var out bytes.Buffer
	n, err := copyWithIdleTimeout(&out, body, 80*time.Millisecond)
	if err != nil {
		t.Fatalf("慢速但持续有数据不该中断: %v", err)
	}
	if got, want := out.String(), "aaaabbbbccccddddeeee"; got != want {
		t.Fatalf("内容=%q want %q", got, want)
	}
	if n != int64(len(out.String())) {
		t.Fatalf("n=%d want %d", n, len(out.String()))
	}
}

// TestReadAllWithIdleTimeout 正常读完返回全部内容。
func TestReadAllWithIdleTimeout(t *testing.T) {
	body := newFakeBody(time.Millisecond, false, "hello-", "catcher")
	got, err := readAllWithIdleTimeout(body, time.Second)
	if err != nil {
		t.Fatalf("readAllWithIdleTimeout: %v", err)
	}
	if string(got) != "hello-catcher" {
		t.Fatalf("got=%q", got)
	}
}

// TestClientHasNoTotalTimeout 共享客户端不得再设总时长超时——
// 那是「大文件慢链路必死」的根因，改回它等于回退本次修复。
func TestClientHasNoTotalTimeout(t *testing.T) {
	oldClient := testStd.sharedClient
	testStd.netMu.Lock()
	testStd.sharedClient = nil
	testStd.netMu.Unlock()
	t.Cleanup(func() {
		testStd.netMu.Lock()
		testStd.sharedClient = oldClient
		testStd.netMu.Unlock()
	})

	c := testStd.getClient()
	if c.Timeout != 0 {
		t.Fatalf("http.Client.Timeout=%v want 0（改用分层/空闲超时）", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型=%T want *http.Transport", c.Transport)
	}
	// 头部超时必须存在：否则「服务端收下请求就不吭声」会永久挂住。
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("ResponseHeaderTimeout 未设置")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Fatal("TLSHandshakeTimeout 未设置")
	}
}
