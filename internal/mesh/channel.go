// Package mesh 实现多机互联：同一局域网内多台 PC 上的 serialtap 守护进程
// 通过 UDP beacon 互发现，经预共享密钥（PSK）加密的 TCP 信道互相转发控制
// 请求 —— 在任意一台 PC 上即可管理所有 PC 接入的嵌入式主板。
//
// 安全模型：mesh 信道上会流过刷机镜像、esptool 日志乃至 NVS 提取内容
// （可能含真实凭据），因此必须加密而非裸 token。口令经 PBKDF2 派生主钥，
// 每连接随机 salt + HKDF 派生双向子钥，帧体 AES-256-GCM 密封；能解开即
// 证明对方持钥（隐式双向认证）。beacon 只携带主钥的 HMAC 指纹，不同密钥
// 的 mesh 互不可见且不泄密钥。
package mesh

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// 帧类型（1 字节，明文——路由分发需要先看类型；类型本身不是秘密）。
const (
	ftHello = 'H' // 明文握手: {"v","salt"}（仅客户端→服务端第一帧）
	ftIdent = 'I' // 密封自报: 客户端 {"id","name","ts"} / 服务端 {"id","name"}
	ftPing  = 'P' // 密封保活探测: {"t"} → ftPong
	ftPong  = 'Q'
	ftReq   = 'R' // 密封请求:  {"id":n,"req":ctl.Request}
	ftResp  = 'S' // 密封响应:  {"id":n,"resp":ctl.Response}（流式命令多条同 id）
	ftUp    = 'U' // 密封上传分块: {"sid","name","seq","total","b64"}
	ftUpAck = 'A' // 密封上传确认: {"sid","name","last_seq"}
	ftDown  = 'D' // 密封下载请求: {"token","name"}（token 限定本会话产物）
	ftBlock = 'B' // 密封数据块: {"b64","eof"}（下载/尾随流共用）
	ftTail  = 'T' // 密封尾随请求: {"device","kind"}
	ftDial  = 'X' // 密封隧道拨号: {"key"} → ftDialAck
	ftDialA = 'Y' // 密封隧道确认: {"ok","device","device_key","err"}；此后双向均为 ftWire
	ftWire  = 'W' // 密封裸字节（隧道模式，payload=串口字节流）
	ftPair  = 'Z' // 密封配对/链接: {"id","name","port"} → ftPairAck
	ftPairA = 'z' // 密封配对应答: {"id","name","approved","auto"}
)

const (
	protoVersion  = 1
	maxFrame      = 8 << 20 // 单帧上限 8MB（上传/下载分块按 1MB 原始字节走，b64 后仍在限内）
	handshakeTO   = 5 * time.Second
	skewTolerance = 10 * time.Minute // 自报 ts 容差（PC 间时钟偏移）
)

// pbkdf2Iters: 口令派生迭代数。生产 600k（OWASP 2023 对 pbkdf2-sha256 的
// 建议），进程内只派生一次（~0.3s）；测试下调加速。
var pbkdf2Iters = 600_000

// Secrets: 由口令派生的节点级密钥组（进程内派生一次，所有连接共用）。
type Secrets struct {
	master []byte // 32B，信道密钥根
	fp     []byte // 32B，beacon 指纹 HMAC 密钥（与信道分离）
}

// DeriveSecrets: 口令 → 主钥 + 指纹钥。空口令直接拒绝（防无加密裸奔）。
func DeriveSecrets(passphrase string) (*Secrets, error) {
	return deriveSecrets(passphrase, pbkdf2Iters)
}

func deriveSecrets(passphrase string, iters int) (*Secrets, error) {
	if passphrase == "" {
		return nil, errors.New("mesh_key 为空：mesh 需要预共享密钥口令（serialtap mesh keygen 生成）")
	}
	master, err := pbkdf2.Key(sha256.New, passphrase, []byte("serialtap-mesh-v1"), iters, 32)
	if err != nil {
		return nil, err
	}
	fp, err := hkdf.Key(sha256.New, master, nil, "serialtap-mesh/fp", 32)
	if err != nil {
		return nil, err
	}
	return &Secrets{master: master, fp: fp}, nil
}

// Fingerprint: 节点身份的密钥指纹（HMAC-SHA256(fp, id) 前 8 字节 hex）。
// 进 beacon 明文——同 mesh 的节点可互认，旁观者拿不到口令。
func (s *Secrets) Fingerprint(id string) string {
	mac := hmac.New(sha256.New, s.fp)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

// ValidFingerprint: beacon 的指纹是否与本节点同 mesh。
func (s *Secrets) ValidFingerprint(id, fp string) bool {
	want := s.Fingerprint(id)
	return len(fp) == len(want) && subtle.ConstantTimeCompare([]byte(want), []byte(fp)) == 1
}

// Ident: 节点身份（握手自报）。
type Ident struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port,omitempty"` // 自报 mesh 监听端口（来话方登记用；0=未报）
	TS   int64  `json:"ts,omitempty"`   // 客户端自报携带（Unix 秒）
}

// helloFrame / identFrame: 握手载荷。
type helloFrame struct {
	V    int    `json:"v"`
	Salt string `json:"salt"` // b64(16B)
}
type identFrame struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port,omitempty"`
	TS   int64  `json:"ts"`
}

// Channel: 一条已握手的加密信道。Send/Recv 各自并发安全（Send 互斥串行；
// Recv 只应在单读循环里调用）。
type Channel struct {
	conn  net.Conn
	aeads [2]cipher.AEAD // [0]=客户端→服务端方向, [1]=服务端→客户端方向
	role  int            // 0=客户端（发送用 aeads[0]）, 1=服务端
	tx    uint64         // 发送帧序（AAD 防重排/重放）
	rx    uint64         // 期望的接收帧序
	wmu   sync.Mutex
}

// deriveConnKeys: 每连接 salt → 双向子钥。
func deriveConnKeys(master, salt []byte) (c2s, s2c cipher.AEAD, err error) {
	kc, err := hkdf.Key(sha256.New, master, salt, "serialtap-mesh/c2s", 32)
	if err != nil {
		return nil, nil, err
	}
	ks, err := hkdf.Key(sha256.New, master, salt, "serialtap-mesh/s2c", 32)
	if err != nil {
		return nil, nil, err
	}
	if c2s, err = newAEAD(kc); err != nil {
		return nil, nil, err
	}
	if s2c, err = newAEAD(ks); err != nil {
		return nil, nil, err
	}
	return c2s, s2c, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	bl, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(bl)
}

// DialChannel: 客户端——拨号并完成握手，返回就绪信道与对端身份。
// 任何失败都关闭连接（对端不会替我们收拾）。
func DialChannel(conn net.Conn, sec *Secrets, self Ident) (ch *Channel, peer Ident, err error) {
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(handshakeTO))
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, Ident{}, err
	}
	hello, _ := json.Marshal(helloFrame{V: protoVersion, Salt: b64(salt)})
	if err = writeFrame(conn, ftHello, hello); err != nil {
		return nil, Ident{}, fmt.Errorf("mesh 握手发送失败: %w", err)
	}
	var c2s, s2c cipher.AEAD
	if c2s, s2c, err = deriveConnKeys(sec.master, salt); err != nil {
		return nil, Ident{}, err
	}
	ch = &Channel{conn: conn, aeads: [2]cipher.AEAD{c2s, s2c}, role: 0}

	self.TS = time.Now().Unix()
	ib, _ := json.Marshal(identFrame(self))
	if err = ch.send(ftIdent, ib); err != nil {
		return nil, Ident{}, err
	}
	var t byte
	var got []byte
	t, got, err = ch.recv()
	if err != nil {
		return nil, Ident{}, fmt.Errorf("mesh 握手失败（密钥不匹配或对端不可用）: %w", err)
	}
	if t != ftIdent {
		return nil, Ident{}, fmt.Errorf("mesh 握手阶段收到意外帧 %q", t)
	}
	var peerRaw identFrame
	if err = json.Unmarshal(got, &peerRaw); err != nil {
		return nil, Ident{}, err
	}
	if peerRaw.ID == "" || peerRaw.Name == "" {
		return nil, Ident{}, errors.New("mesh 对端身份不完整")
	}
	_ = conn.SetDeadline(time.Time{})
	return ch, Ident{ID: peerRaw.ID, Name: peerRaw.Name, Port: peerRaw.Port}, nil
}

// AcceptChannel: 服务端——在已接受的连接上等待并完成握手。
// 失败时关闭连接（客户端可能还挂着等回复）。
func AcceptChannel(conn net.Conn, sec *Secrets, self Ident) (ch *Channel, peer Ident, err error) {
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(handshakeTO))
	var t byte
	var payload []byte
	t, payload, err = readFrame(conn)
	if err != nil {
		return nil, Ident{}, fmt.Errorf("mesh 握手读取失败: %w", err)
	}
	if t != ftHello {
		return nil, Ident{}, fmt.Errorf("mesh 期待 hello 帧得到 %q", t)
	}
	var hello helloFrame
	if err = json.Unmarshal(payload, &hello); err != nil {
		return nil, Ident{}, err
	}
	if hello.V != protoVersion {
		return nil, Ident{}, fmt.Errorf("mesh 协议版本不兼容: %d", hello.V)
	}
	var salt []byte
	if salt, err = unb64(hello.Salt); err != nil || len(salt) == 0 {
		return nil, Ident{}, errors.New("mesh hello salt 非法")
	}
	var c2s, s2c cipher.AEAD
	if c2s, s2c, err = deriveConnKeys(sec.master, salt); err != nil {
		return nil, Ident{}, err
	}
	ch = &Channel{conn: conn, aeads: [2]cipher.AEAD{c2s, s2c}, role: 1}

	var got []byte
	t, got, err = ch.recv()
	if err != nil {
		return nil, Ident{}, fmt.Errorf("mesh 握手失败（密钥不匹配）: %w", err)
	}
	if t != ftIdent {
		return nil, Ident{}, fmt.Errorf("mesh 握手阶段收到意外帧 %q", t)
	}
	var peerRaw identFrame
	if err = json.Unmarshal(got, &peerRaw); err != nil {
		return nil, Ident{}, err
	}
	if peerRaw.ID == "" || peerRaw.Name == "" {
		return nil, Ident{}, errors.New("mesh 对端身份不完整")
	}
	if d := time.Since(time.Unix(peerRaw.TS, 0)); d > skewTolerance || d < -skewTolerance {
		return nil, Ident{}, fmt.Errorf("mesh 对端时钟偏差过大（%s），请校时", d.Truncate(time.Second))
	}
	if err = ch.send(ftIdent, mustJSON(Ident{ID: self.ID, Name: self.Name})); err != nil {
		return nil, Ident{}, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ch, Ident{ID: peerRaw.ID, Name: peerRaw.Name, Port: peerRaw.Port}, nil
}

// Send: 发送一帧（自动密封）。'H' 之外的明文帧不存在——握手后全部密封。
func (c *Channel) Send(t byte, payload []byte) error {
	return c.send(t, payload)
}

func (c *Channel) send(t byte, plain []byte) error {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	var aad [9]byte
	aad[0] = t
	binary.BigEndian.PutUint64(aad[1:], c.tx)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.tx++
	ct := c.aeads[c.role].Seal(nil, nonce, plain, aad[:])
	buf := make([]byte, 4, 5+len(nonce)+len(ct))
	binary.BigEndian.PutUint32(buf, uint32(1+len(nonce)+len(ct)))
	buf = append(append(append(buf, t), nonce...), ct...)
	_, err := c.conn.Write(buf)
	return err
}

// Recv: 阻塞接收一帧（自动解封）。帧序不符（重放/重排）即报错，调用方应关闭信道。
func (c *Channel) Recv() (byte, []byte, error) {
	return c.recv()
}

func (c *Channel) recv() (byte, []byte, error) {
	t, payload, err := readFrame(c.conn)
	if err != nil {
		return 0, nil, err
	}
	if len(payload) < 12+16 { // nonce + GCM tag
		return 0, nil, fmt.Errorf("mesh 帧过短（类型 %q）", t)
	}
	var aad [9]byte
	aad[0] = t
	binary.BigEndian.PutUint64(aad[1:], c.rx)
	c.rx++
	plain, err := c.aeads[1-c.role].Open(nil, payload[:12], payload[12:], aad[:])
	if err != nil {
		return 0, nil, fmt.Errorf("mesh 帧解封失败（类型 %q，序 %d）: %w", t, c.rx-1, err)
	}
	return t, plain, nil
}

// PeerAddr: 对端网络地址（审计/展示）。
func (c *Channel) PeerAddr() string { return c.conn.RemoteAddr().String() }

// Close: 关闭底层连接。
func (c *Channel) Close() error { return c.conn.Close() }

// —— 帧读写（长度前缀：4B 大端 = 类型 1B + 载荷） ——

func writeFrame(conn net.Conn, t byte, payload []byte) error {
	buf := make([]byte, 4, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)))
	buf = append(append(buf, t), payload...)
	_, err := conn.Write(buf)
	return err
}

func readFrame(conn net.Conn) (byte, []byte, error) {
	var lb [4]byte
	if _, err := io.ReadFull(conn, lb[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n == 0 || n > maxFrame {
		return 0, nil, fmt.Errorf("mesh 帧长非法: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
