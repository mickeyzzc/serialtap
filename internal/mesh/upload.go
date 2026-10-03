package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/flash"
)

// 上传/下载：远程刷机的镜像与 NVS dump 产物走 mesh 加密信道分块传输。
// 服务端只落 .flash-upload/mesh-<sid>/（刷机镜像）与 .mesh-share/（dump
// 产物）；文件名白名单化；下载只认 dlAuth 登记过的路径——持钥者不能借
// 此读 daemon 用户的任意文件。

const uploadChunk = 1 << 20 // 1MB 原始字节/块

// upFrame: ftUp 载荷。seq 从 0 起。
type upFrame struct {
	SID   string `json:"sid"`
	Name  string `json:"name"`
	Seq   int    `json:"seq"`
	Total int    `json:"total"`
	B64   string `json:"b64"`
}
type upAckFrame struct {
	SID     string `json:"sid"`
	Name    string `json:"name"`
	LastSeq int    `json:"last_seq"` // -1 = 拒绝（非法名/会话）
	Path    string `json:"path"`     // 服务端落盘绝对路径（客户端据此改写刷机 spec）
}
type downFrame struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}
type blockFrame struct {
	B64 string `json:"b64"`
	EOF bool   `json:"eof"`
}

var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// handleUpload: 服务端——分块追加落盘，逐块应答（客户端按 ack 流控）。
// 同名文件按到达顺序追加；同一连接顺序发送即天然有序。
func (n *Node) handleUpload(ch *Channel, payload []byte) error {
	var uf upFrame
	if err := json.Unmarshal(payload, &uf); err != nil {
		return err
	}
	if !safeNameRe.MatchString(uf.Name) || !safeNameRe.MatchString(uf.SID) {
		return ch.Send(ftUpAck, mustJSON(upAckFrame{SID: uf.SID, Name: uf.Name, LastSeq: -1}))
	}
	dir := filepath.Join(n.opt.Root, ".flash-upload", "mesh-"+uf.SID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, uf.Name)
	data, err := unb64(uf.B64)
	if err != nil {
		return err
	}
	if uf.Seq == 0 {
		// 新文件（同名重传 = 追加语义靠 seq 顺序；seq0 截断重建）
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	} else {
		f, oerr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if oerr != nil {
			return oerr
		}
		_, err = f.Write(data)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	n.logf("[mesh] 上传分块 %s/%s seq=%d（%dB）", uf.SID, uf.Name, uf.Seq, len(data))
	return ch.Send(ftUpAck, mustJSON(upAckFrame{SID: uf.SID, Name: uf.Name, LastSeq: uf.Seq, Path: path}))
}

// handleDownload: 服务端——dlAuth token → 分块回传。一次性（发完即撤销 token）。
func (n *Node) handleDownload(ch *Channel, _ context.Context, payload []byte) error {
	var df downFrame
	if err := json.Unmarshal(payload, &df); err != nil {
		return err
	}
	n.mu.Lock()
	path, ok := n.dlAuth[df.Token]
	if ok && (df.Name == "" || df.Name != filepath.Base(path)) {
		ok = false
	}
	if ok {
		delete(n.dlAuth, df.Token)
	}
	n.mu.Unlock()
	if !ok {
		return ch.Send(ftBlock, mustJSON(blockFrame{EOF: true}))
	}
	f, err := os.Open(path)
	if err != nil {
		return ch.Send(ftBlock, mustJSON(blockFrame{EOF: true}))
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, uploadChunk)
	for {
		nr, rerr := f.Read(buf)
		if nr > 0 {
			if serr := ch.Send(ftBlock, mustJSON(blockFrame{B64: b64(buf[:nr])})); serr != nil {
				return serr
			}
		}
		if rerr != nil {
			break
		}
	}
	n.logf("[mesh] 产物下载 %s（%s）", df.Name, ch.PeerAddr())
	return ch.Send(ftBlock, mustJSON(blockFrame{EOF: true}))
}

// uploadSession: 客户端——一条连接上传若干命名文件，返回 name → 远端
// 绝对路径（来自服务端 ack）。逐块等 ack：1MB/块在 LAN 上够快且有背压。
func (n *Node) uploadSession(peerName string, files map[string][]byte) (map[string]string, error) {
	peer, err := n.reg.Match(peerName)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", peer.Addr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连不上 peer %s: %w", peer.NameOrAddr(), err)
	}
	defer func() { _ = conn.Close() }()
	ch, ident, err := DialChannel(conn, n.sec, n.self)
	if err != nil {
		return nil, fmt.Errorf("与 peer %s 握手失败: %w", peerName, err)
	}
	n.reg.Touch(ident.ID)
	if peer.Static || peer.ID == "" {
		n.reg.LearnedIdent(peer.Addr, ident)
	}
	sid := randomHex(8)
	remote := map[string]string{}
	for name, data := range files {
		total := (len(data) + uploadChunk - 1) / uploadChunk
		if total == 0 {
			total = 1
		}
		for seq := 0; seq < total; seq++ {
			lo := seq * uploadChunk
			hi := lo + uploadChunk
			if hi > len(data) {
				hi = len(data)
			}
			if err := ch.Send(ftUp, mustJSON(upFrame{SID: sid, Name: name, Seq: seq, Total: total, B64: b64(data[lo:hi])})); err != nil {
				return nil, err
			}
			t, payload, rerr := ch.Recv()
			if rerr != nil {
				return nil, rerr
			}
			if t != ftUpAck {
				return nil, fmt.Errorf("上传期间收到意外帧 %q", t)
			}
			var ack upAckFrame
			if jerr := json.Unmarshal(payload, &ack); jerr != nil || ack.LastSeq != seq {
				return nil, fmt.Errorf("上传块 %s#%d 未被确认（%v）", name, seq, jerr)
			}
			if ack.LastSeq >= 0 {
				remote[name] = ack.Path
			} else {
				return nil, fmt.Errorf("peer 拒绝上传 %s（文件名/会话非法）", name)
			}
		}
		n.logf("[mesh] 已上传 %s → peer %s（%d B/%d 块）", name, ident.Name, len(data), total)
	}
	return remote, nil
}

// remoteFlash: 客户端——把本机可读的镜像（或 args-file 及其引用的 bins）
// 上传到 peer，再转发 flash（spec 路径已改写为远端落盘路径）。
// CLI 与本地守护同机，"本机可读" 对用户就是 "敲命令的这台机器"。
func (n *Node) remoteFlash(peerName string, req ctl.Request, respond func(ctl.Response)) error {
	spec := req.Spec
	files := map[string][]byte{}
	var bins []flash.BinSpec

	if spec.ArgsFile != "" {
		parsed, chip, err := flash.ParseArgsFile(spec.ArgsFile)
		if err != nil {
			return fmt.Errorf("解析 args-file 失败: %w", err)
		}
		bins = parsed
		if spec.Chip == "" && chip != "" {
			spec.Chip = chip // args-file 的 chip 提示随迁（args 文件本身不上传）
		}
	} else {
		bins = spec.Bins
	}
	dir := ""
	if spec.ArgsFile != "" {
		dir = filepath.Dir(spec.ArgsFile)
	}
	for i, b := range bins {
		p := b.Path
		if dir != "" && !filepath.IsAbs(p) {
			p = filepath.Join(dir, filepath.FromSlash(p))
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("读取 %s 失败（远程刷机要求镜像在本机可读）: %w", p, err)
		}
		name := fmt.Sprintf("bin%d.img", i)
		files[name] = data
		bins[i].Path = name // 暂存上传名，待 ack 换远端路径
	}
	remote, err := n.uploadSession(peerName, files)
	if err != nil {
		return err
	}
	spec.ArgsFile = ""
	spec.Bins = make([]flash.BinSpec, len(bins))
	for i, b := range bins {
		spec.Bins[i] = flash.BinSpec{Path: remote[b.Path], Offset: b.Offset}
	}
	req.Spec = spec
	_, err = n.Call(peerName, req, func(r ctl.Response) bool {
		respond(r)
		return false
	})
	return err
}

// remoteBoardDump: 客户端——OutPath 置空转发（对端落 .mesh-share 并回
// token），产物下载回本机写原始 OutPath（CLI 用户指定的路径是本机语义）。
func (n *Node) remoteBoardDump(peerName string, req ctl.Request, respond func(ctl.Response)) error {
	localOut := req.Board.OutPath
	req.Board.OutPath = "" // mesh 远端模式标记
	ref, err := n.Call(peerName, req, func(r ctl.Response) bool {
		respond(r)
		return false
	})
	if err != nil {
		return err
	}
	if ref == nil {
		return fmt.Errorf("远端 dump 未返回产物引用")
	}
	data, err := n.download(peerName, ref.Token, ref.Name)
	if err != nil {
		return fmt.Errorf("产物取回失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(localOut), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(localOut, data, 0o600); err != nil {
		return err
	}
	respond(ctl.Response{OK: true, Event: "board-log", Line: fmt.Sprintf("产物已取回本机: %s（%d B，0600）", localOut, len(data))})
	return nil
}

// download: 客户端——token 取回产物字节。
func (n *Node) download(peerName, token, name string) ([]byte, error) {
	peer, err := n.reg.Match(peerName)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", peer.Addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ch, ident, err := DialChannel(conn, n.sec, n.self)
	if err != nil {
		return nil, err
	}
	n.reg.Touch(ident.ID)
	if err := ch.Send(ftDown, mustJSON(downFrame{Token: token, Name: name})); err != nil {
		return nil, err
	}
	var out []byte
	for {
		t, payload, rerr := ch.Recv()
		if rerr != nil {
			return nil, rerr
		}
		if t != ftBlock {
			continue
		}
		var bf blockFrame
		if jerr := json.Unmarshal(payload, &bf); jerr != nil {
			return nil, jerr
		}
		if bf.B64 != "" {
			chunk, derr := unb64(bf.B64)
			if derr != nil {
				return nil, derr
			}
			out = append(out, chunk...)
		}
		if bf.EOF {
			return out, nil
		}
	}
}
