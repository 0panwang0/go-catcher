// HLS 分片解密：#EXT-X-KEY 方法注册表 + AES-128 实现。
//
// 与容器注册表（container.go）同款模式：一种加密方案 = 注册表一个条目
// （METHOD 名 + 解密器工厂）。下载管线按播放列表声明的 METHOD 自动装配
// 解密器，新增方案（如 SAMPLE-AES）只需追加条目，无需改动管线代码。
package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// SegDecryptor 单个媒体分片的解密器。
type SegDecryptor interface {
	// DecryptSegment 解密 media sequence 为 seq 的分片。
	// 显式 IV 的方案忽略 seq；按序号派生 IV 的方案（AES-128 无 IV 属性时）用它。
	DecryptSegment(seq uint64, data []byte) ([]byte, error)
}

// KeySpec 构建解密器所需的全部输入（key 内容由调用方拉取，工厂保持纯函数、可单测）。
type KeySpec struct {
	Key []byte // key 服务端返回的原始字节
	IV  []byte // 播放列表显式 IV（16 字节）；nil = 按 media sequence 派生
}

// keyMethodEntry 一种 #EXT-X-KEY METHOD 的注册条目。
type keyMethodEntry struct {
	method string // 标准方法名（大写），如 "AES-128"
	create func(spec KeySpec) (SegDecryptor, error)
}

// keyMethodRegistry 已支持的加密方法，新增方案在此追加条目即可。
var keyMethodRegistry = []keyMethodEntry{
	{method: "AES-128", create: newAES128Decryptor},
}

// findKeyMethod 按方法名查找注册条目（大小写不敏感）；未注册返回 nil。
func findKeyMethod(method string) *keyMethodEntry {
	name := strings.ToUpper(strings.TrimSpace(method))
	for i := range keyMethodRegistry {
		if keyMethodRegistry[i].method == name {
			return &keyMethodRegistry[i]
		}
	}
	return nil
}

// normKeyFormat 归一化 KEYFORMAT 用于比较：缺省（空串）与显式 "identity" 是
// 同一件事（RFC 8216 规定缺省即 identity），大小写也不敏感。
//
// 必须走归一化再比：一条把首条 key 裸写、后续重复时写了 KEYFORMAT="identity"
// 的播放列表完全合法，按原始字符串比较会被误判成 key rotation 而拒绝下载。
func normKeyFormat(f string) string {
	f = strings.ToLower(strings.TrimSpace(f))
	if f == "" {
		return "identity"
	}
	return f
}

// isIdentityKeyFormat 报告 KEYFORMAT 是否为 identity（含缺省）。
//
// 只有 identity 形态（URI 直接返回裸密钥字节）才是本工具能处理的；其余
// （com.apple.streamingkeydelivery、urn:uuid:edef8ba9-… 等）代表 DRM 密钥系统，
// 需要许可证才能拿到真密钥。
func isIdentityKeyFormat(f string) bool {
	return normKeyFormat(f) == "identity"
}

// ensureDecryptor 按播放列表声明的密钥装配分片解密器。
// 幂等：key 指纹不变不重建（直播每轮轮询重复调用也只拉一次 key）；
// 明文播放列表（key 为 nil）清空现有解密器。
func (j *dlJob) ensureDecryptor(ctx context.Context, key *KeyInfo) error {
	if key == nil {
		j.decryptor = nil
		j.decKeyID = ""
		return nil
	}
	fp := key.fingerprint()
	if j.decryptor != nil && fp == j.decKeyID {
		return nil
	}
	entry := findKeyMethod(key.Method)
	if entry == nil {
		return fmt.Errorf("播放列表使用了不支持的加密方法 %q（当前支持 AES-128）", key.Method)
	}
	keyData, err := fetchSegment(ctx, j, key.URI)
	if err != nil {
		return fmt.Errorf("获取解密密钥失败: %w", err)
	}
	d, err := entry.create(KeySpec{Key: keyData, IV: key.IV})
	if err != nil {
		return fmt.Errorf("构建 %s 解密器失败: %w", key.Method, err)
	}
	j.decryptor = d
	j.decKeyID = fp
	return nil
}

// decryptSegmentIfAny 对单个已下载分片解密（无解密器时原样返回）。
// 容器探测预取的分片（j.pre）在上游解密，不经此方法。
func (j *dlJob) decryptSegmentIfAny(seq uint64, data []byte) ([]byte, error) {
	if j.decryptor == nil {
		return data, nil
	}
	return j.decryptor.DecryptSegment(seq, data)
}

// ============================================================
// AES-128（CBC，RFC 8216 §5.2）
// ============================================================

type aes128Decryptor struct {
	block   cipher.Block
	fixedIV []byte // 显式 IV；nil = 按 media sequence 派生
}

func newAES128Decryptor(spec KeySpec) (SegDecryptor, error) {
	if len(spec.Key) != aes.BlockSize {
		return nil, fmt.Errorf("密钥长度 %d 字节，AES-128 要求 16 字节", len(spec.Key))
	}
	block, err := aes.NewCipher(spec.Key)
	if err != nil {
		return nil, err
	}
	d := &aes128Decryptor{block: block}
	if len(spec.IV) == aes.BlockSize {
		d.fixedIV = spec.IV
	}
	return d, nil
}

// DecryptSegment 每个分片独立 CBC 解密（HLS 语义：分片间不链式，各自以 IV 起块）。
// PKCS7 尾部填充不剥离：hls.js/ffmpeg 同样保留，TS/MP4 解复用器会忽略尾部残片；
// 猜测式剥离反而可能吃掉合法数据（明文恰好以 0x01 结尾的 TS 包）。
func (d *aes128Decryptor) DecryptSegment(seq uint64, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文长度 %d 不是 16 字节块对齐", len(data))
	}
	iv := d.fixedIV
	if iv == nil {
		iv = mediaSeqIV(seq)
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(d.block, iv).CryptBlocks(plain, data)
	return plain, nil
}

// mediaSeqIV 按分片 media sequence 派生 IV：序号的 16 字节大端表示
// （RFC 8216 §5.2：IV 属性缺省时的规定行为）。
func mediaSeqIV(seq uint64) []byte {
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], seq)
	return iv
}

// fingerprint key 指纹（方法+URI+IV），ensureDecryptor 幂等去重用。
func (k *KeyInfo) fingerprint() string {
	return k.Method + "|" + k.URI + "|" + hex.EncodeToString(k.IV) + "|" + normKeyFormat(k.KeyFormat)
}
