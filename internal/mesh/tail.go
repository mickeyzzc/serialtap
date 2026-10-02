package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// 远程日志尾随：peer 侧按 web 面板同款协议（最新文件 + 增量差分 + 轮转
// 跟随）读 root/<name>/<kind>-YYYYMMDD[.NNN].log，增量以 ftBlock 帧推回；
// 空 b64 块 = 15s 保活。客户端 TailStream 回调消费字节。

const (
	tailPoll    = 700 * time.Millisecond
	tailInitMax = 16 << 10 // 初始尾部最多回放 16KB（与面板一致）
	tailPing    = 15 * time.Second
)

type tailFrame struct {
	Device string `json:"device"`
	Kind   string `json:"kind"` // serial | events
}

var tailKindRe = regexp.MustCompile(`^(serial|events)$`)

// latestTailFile: 设备目录下最新的 kind-*.log（轮转感知排序，与面板同规则）。
func latestTailFile(root, device, kind string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(root, device, kind+"-*.log"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	sort.Slice(matches, func(i, j int) bool {
		// 无后缀的当日主文件 > 带序号后缀；同后缀按名字
		pi, pj := matches[i], matches[j]
		ei, eji := len(pi), len(pj)
		for ei > 0 && pi[ei-1] >= '0' && pi[ei-1] <= '9' {
			ei--
		}
		for eji > 0 && pj[eji-1] >= '0' && pj[eji-1] <= '9' {
			eji--
		}
		if ei != eji {
			return ei < eji // 主文件（更短）在后缀比较时更大
		}
		return pi > pj
	})
	return matches[len(matches)-1], true
}

// handleTail: peer 侧——尾随推送直到连接关闭（ctx 取消/写失败即止）。
func (n *Node) handleTail(ch *Channel, ctx context.Context, payload []byte) error {
	var tf tailFrame
	if err := json.Unmarshal(payload, &tf); err != nil {
		return err
	}
	if !safeNameRe.MatchString(tf.Device) || !tailKindRe.MatchString(tf.Kind) {
		return ch.Send(ftBlock, mustJSON(blockFrame{EOF: true}))
	}
	n.logf("[mesh] 远程尾随 %s/%s（%s）", tf.Device, tf.Kind, ch.PeerAddr())
	send := func(b []byte) bool {
		return ch.Send(ftBlock, mustJSON(blockFrame{B64: b64(b)})) == nil
	}

	var file string
	var offset int64
	lastPing := time.Now()
	emit := func() bool {
		cur, ok := latestTailFile(n.opt.Root, tf.Device, tf.Kind)
		if !ok {
			return true // 无日志：等待设备出现
		}
		if cur != file {
			file = cur
			offset = 0
		}
		fi, err := os.Stat(file)
		if err != nil {
			return true
		}
		if fi.Size() < offset { // 轮转/清空：从头跟新文件
			offset = 0
		}
		if fi.Size() > offset {
			f, err := os.Open(file)
			if err != nil {
				return true
			}
			if file == cur && offset == 0 && fi.Size() > tailInitMax {
				offset = fi.Size() - tailInitMax // 首见只回放尾部 16KB
				_, _ = f.Seek(offset, 0)
			} else {
				_, _ = f.Seek(offset, 0)
			}
			buf := make([]byte, 1<<20)
			for {
				nr, rerr := f.Read(buf)
				if nr > 0 {
					offset += int64(nr)
					if !send(buf[:nr]) {
						_ = f.Close()
						return false
					}
				}
				if rerr != nil {
					break
				}
			}
			_ = f.Close()
		}
		return true
	}

	tick := time.NewTicker(tailPoll)
	defer tick.Stop()
	for {
		if !emit() {
			return nil
		}
		if time.Since(lastPing) >= tailPing {
			lastPing = time.Now()
			if !send(nil) { // 空块保活
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// TailStream: 客户端——订阅 peer 上某设备的日志尾随。onData 收到增量字节；
// ctx 取消或连接断开即返回。空块（保活）不回调。
func (n *Node) TailStream(ctx context.Context, peerName, device, kind string, onData func([]byte)) error {
	peer, err := n.reg.Match(peerName)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", peer.Addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("连不上 peer %s: %w", peer.NameOrAddr(), err)
	}
	defer func() { _ = conn.Close() }()
	ch, ident, err := DialChannel(conn, n.sec, n.self)
	if err != nil {
		return fmt.Errorf("与 peer %s 握手失败: %w", peerName, err)
	}
	n.reg.Touch(ident.ID)
	if err := ch.Send(ftTail, mustJSON(tailFrame{Device: device, Kind: kind})); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ch.Close() // 解除 Recv 阻塞
	}()
	for {
		t, payload, rerr := ch.Recv()
		if rerr != nil || ctx.Err() != nil {
			return nil // 尾随是无限流：取消/断链都算正常结束
		}
		if t != ftBlock {
			continue
		}
		var bf blockFrame
		if err := json.Unmarshal(payload, &bf); err != nil {
			continue
		}
		if bf.B64 != "" {
			if chunk, derr := unb64(bf.B64); derr == nil {
				onData(chunk)
			}
		}
		if bf.EOF {
			return nil
		}
	}
}
