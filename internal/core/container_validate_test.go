// 容器校验注册表（批次2）：探测期守卫与落盘前抽样共用同一份判据。
package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateTS 校验器必须"确证损坏"而不是"确证正常"：
// 网格命中率过低才算坏，短样本一律放行。
func TestValidateTS(t *testing.T) {
	valid := make([]byte, 188*8)
	for i := 0; i < len(valid); i += 188 {
		valid[i] = 0x47
	}
	garbage := make([]byte, 188*8) // 全 0：没有任何同步字节
	short := []byte{0x47, 0x00, 0x00}

	c := findContainerByID("ts")
	if err := runValidator(c, valid, nil, ProbeInfo{}); err != nil {
		t.Fatalf("合法 TS 被误判: %v", err)
	}
	if err := runValidator(c, garbage, nil, ProbeInfo{}); err == nil {
		t.Fatal("无同步字节的样本应判损坏")
	}
	if err := runValidator(c, short, nil, ProbeInfo{}); err != nil {
		t.Fatalf("短样本应放行（无法判断）: %v", err)
	}
	// 中部样本可独立提供证据
	if err := runValidator(c, short, valid, ProbeInfo{}); err != nil {
		t.Fatalf("中部样本合法应放行: %v", err)
	}
}

// TestValidateFMP4 box 头识别：ftyp/moov/styp/moof 开头放行，无 box 特征判损坏。
func TestValidateFMP4(t *testing.T) {
	c := findContainerByID("fmp4")
	for _, typ := range []string{"ftyp", "moov", "styp", "moof"} {
		head := append([]byte{0, 0, 0, 0x20}, []byte(typ)...)
		if err := runValidator(c, head, nil, ProbeInfo{}); err != nil {
			t.Fatalf("%s 开头应放行: %v", typ, err)
		}
	}
	if err := runValidator(c, []byte(strings.Repeat("A", 64)), nil, ProbeInfo{}); err == nil {
		t.Fatal("无 box 特征的样本应判损坏")
	}
	if err := runValidator(c, []byte("abc"), nil, ProbeInfo{}); err != nil {
		t.Fatalf("过短样本应放行: %v", err)
	}
}

// TestValidateOutputSizeFloor MinBytes 兜住"只落了 init 段、分片一个没写"的假成功。
func TestValidateOutputSizeFloor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.mp4")
	data := make([]byte, 384)
	copy(data[4:8], "ftyp") // 过 validateFMP4 的 box 头判定，隔离出体量下限这条判据
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
	c := findContainerByID("fmp4")
	// 文件 384 字节，init 段也是 384 → 没有媒体分片写入
	if err := validateOutput(c, p, ProbeInfo{MinBytes: 384}); err == nil {
		t.Fatal("大小未超过 init 段应判损坏")
	}
	// 文件大于 init 段 → 通过
	if err := validateOutput(c, p, ProbeInfo{MinBytes: 100}); err != nil {
		t.Fatalf("超过 init 段应放行: %v", err)
	}
	// 不传 MinBytes（TS 等无 init 段容器）不做体量判断
	if err := validateOutput(c, p, ProbeInfo{}); err != nil {
		t.Fatalf("MinBytes=0 不应做体量判断: %v", err)
	}
}

// TestValidateOutputMissingFile 读不到文件时放行——校验器只负责确证损坏。
func TestValidateOutputMissingFile(t *testing.T) {
	c := findContainerByID("ts")
	if err := validateOutput(c, filepath.Join(t.TempDir(), "nope.ts"), ProbeInfo{MinBytes: 999}); err != nil {
		t.Fatalf("文件不存在应放行: %v", err)
	}
}

// TestRegistryValidatorsOnlyWhereMeaningful 只有能给出明确判据的容器才挂校验器；
// 其余格式（无根据判断）保持 nil，避免凭空误伤。
func TestRegistryValidatorsOnlyWhereMeaningful(t *testing.T) {
	has := map[string]bool{}
	for _, c := range containerRegistry {
		if c.Validate != nil {
			has[c.ID] = true
		}
	}
	for _, id := range []string{"ts", "fmp4", "generic"} {
		if !has[id] {
			t.Fatalf("%s 应有校验器", id)
		}
	}
	for _, id := range []string{"flv", "webm", "mkv", "aac", "mp3", "wav", "ogg", "avi"} {
		if has[id] {
			t.Fatalf("%s 尚无可靠判据，不应挂校验器（会凭空误伤）", id)
		}
	}
}
