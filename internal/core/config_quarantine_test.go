// R7：配置读取 / 解析失败必须留档（.bak），且留档必须发生在"回退默认 / 写回"之前。
//
// 缺陷形态：loadConfig 对读失败与解析失败都静默回退默认，随后 saveConfigLocked
// （ensureAPIToken 触发）把默认值写回同一个路径 —— 静默销毁用户改过的设置
// （并发数 / 代理…）。**回退默认本身是既有行为**（TestLoadConfigFallsBack 钉着），
// 缺陷在"无留档 + 会被覆盖 + 无任何可见提示"。
package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func globConfigBak(t *testing.T, p string) []string {
	t.Helper()
	m, err := filepath.Glob(p + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLoadConfigQuarantinesBrokenFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)
	// persistHealth 是共享状态，记了通知要在本用例结束后清掉，免得污染后续用例。
	// ⚠️ 必须用 resetPersistHealth 而不是 recordPersistResult(nil)：后者清不掉
	// notice 槽位（这正是不通知被冲掉的原因）。
	resetPersistHealth(t)

	broken := []byte(`{"maxConcurrent":5,"proxy":"http://127.0.0.1:8888"`) // 截断的 JSON
	if err := os.WriteFile(p, broken, 0644); err != nil {
		t.Fatal(err)
	}
	testStd.loadConfig()

	// 1) 原文件必须被留档（内容一字不差），主路径空出来
	baks := globConfigBak(t, p)
	if len(baks) != 1 {
		t.Fatalf("坏配置应留下恰好一份 .bak 留档，实际 %v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); !bytes.Equal(b, broken) {
		t.Fatalf("留档内容被改动（那不是留档）: %s", b)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("坏配置应从主路径挪走（否则会被默认值覆盖）: %v", err)
	}

	// 2) 回退默认（既有行为不变）
	if testStd.cfg.MaxConcurrent != defaultConfig().MaxConcurrent {
		t.Fatalf("解析失败应回退默认: %+v", testStd.cfg)
	}

	// 3) 留档后必须能正常写回（不能一直卡在"不敢写"）
	if err := testStd.saveConfigLocked(); err != nil {
		t.Fatalf("留档后应能正常保存配置: %v", err)
	}

	// 4) /status 必须看得到（GUI 是 windowsgui、无控制台，只 Printf 等于没写），
	//    且必须**活得过后续的成功落盘** —— 后半句才是 S1 的判据：写进 lastErr 槽位
	//    的话，saveState 成功时的 recordPersistResult(nil) 会把它冲掉，用户一个字都看不到。
	//    （旧断言在 loadConfig 之后立刻检查，只证明"写得进去"、不能证明"留得住"。）
	if d := testStd.persistDTOOf(); d.Notice == "" {
		t.Fatalf("配置留档应经 persistHealth 的 notice 上报 /status，实际: %+v", d)
	}
	testStd.saveState() // 一次真正成功的落盘
	if d := testStd.persistDTOOf(); d.Notice == "" {
		t.Fatalf("配置留档通知被一次成功落盘冲掉了（S1）: %+v", d)
	}
}

// 成对的另一半：**合法**配置不该被留档（否则上面的断言可以靠"永远留档"骗过）。
func TestLoadConfigValidFileIsNotQuarantined(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)

	good := []byte(`{"maxConcurrent":5,"segConcurrency":20,"maxRetries":7}`)
	if err := os.WriteFile(p, good, 0644); err != nil {
		t.Fatal(err)
	}
	testStd.loadConfig()

	if baks := globConfigBak(t, p); len(baks) != 0 {
		t.Fatalf("合法配置不该被留档，实际 %v", baks)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, good) {
		t.Fatalf("合法配置不该被改动: %s", b)
	}
	if testStd.cfg.MaxConcurrent != 5 {
		t.Fatalf("合法配置应被采纳: %+v", testStd.cfg)
	}
}
