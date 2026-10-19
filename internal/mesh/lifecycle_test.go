package mesh

// TDD（#70）：产物与令牌生命周期——dlAuth token 带 TTL（过期拒绝 + 懒清
// + 周期清扫）、dump 产物目录下载后即清、mesh 刷机会话目录随命令完成清理、
// 启动清扫 24h 陈旧目录。背景：token/目录此前只增不清，孤儿产物（可能含
// NVS 敏感内容）在 root 下无限累积。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/board"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// dumpForward: A 侧 fake handler——board dump 把固定内容写入 OutPath。
func dumpForward(t *testing.T) ctl.Handler {
	t.Helper()
	return func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd == "board" && req.Board != nil && req.Board.Action == "dump" && req.Board.OutPath != "" {
			if err := os.MkdirAll(filepath.Dir(req.Board.OutPath), 0o700); err != nil {
				respond(ctl.Response{OK: false, Event: "board-done", Error: err.Error()})
				return
			}
			if err := os.WriteFile(req.Board.OutPath, []byte("NVS-DATA"), 0o600); err != nil {
				respond(ctl.Response{OK: false, Event: "board-done", Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Event: "board-log", Line: "dumped"})
			respond(ctl.Response{OK: true, Event: "board-done"})
			return
		}
		respond(ctl.Response{OK: false, Error: "unexpected"})
	}
}

// 下载完成后产物目录即清、token 撤销。
func TestDumpArtifactDirRemovedAfterDownload(t *testing.T) {
	p := newNodePair(t, dumpForward(t), nil)
	localOut := filepath.Join(t.TempDir(), "fetched", "dump.bin")
	err := p.b.Forward("bench-a", ctl.Request{
		Cmd: "board", Pattern: "^dev$",
		Board: &board.Spec{Action: "dump", Addr: "0x9000", Size: "0x100", OutPath: localOut},
	}, func(ctl.Response) {})
	if err != nil {
		t.Fatalf("remoteBoardDump 失败: %v", err)
	}
	if got := testutil.ReadFile(t, localOut); got != "NVS-DATA" {
		t.Fatalf("取回内容错误: %q", got)
	}
	// A 侧 token 表即时撤销；目录清理由下载 handler 收尾（异步于客户端返回）
	p.a.mu.Lock()
	tokens := len(p.a.dlAuth)
	p.a.mu.Unlock()
	if tokens != 0 {
		t.Fatalf("下载后 token 应撤销: %d", tokens)
	}
	testutil.WaitFor(t, 2*time.Second, func() bool {
		entries, err := os.ReadDir(filepath.Join(p.rootA, ".mesh-share"))
		return err == nil && len(entries) == 0
	}, "下载完成后 .mesh-share 应清空")
}

// 过期 token 拒绝下载且懒清（token 摘除 + 产物目录删除）；新鲜 token 照常。
func TestDownloadTokenTTL(t *testing.T) {
	p := newNodePair(t, dumpForward(t), nil)

	mkArtifact := func(name string) string {
		dir := filepath.Join(p.rootA, ".mesh-share", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dump.bin"), []byte("NVS-STALE"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	staleDir := mkArtifact("aaaa1111")
	freshDir := mkArtifact("bbbb2222")
	p.a.mu.Lock()
	p.a.dlAuth["tok-stale"] = dlGrant{path: filepath.Join(staleDir, "dump.bin"), at: time.Now().Add(-dlAuthTTL - time.Minute)}
	p.a.dlAuth["tok-fresh"] = dlGrant{path: filepath.Join(freshDir, "dump.bin"), at: time.Now()}
	p.a.mu.Unlock()

	// 过期：空回（仅 EOF）+ token/目录懒清
	if got, err := p.b.download("bench-a", "tok-stale", "dump.bin"); err != nil || len(got) != 0 {
		t.Fatalf("过期 token 必须拒绝: %d B err=%v", len(got), err)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("过期产物目录应懒清: %v", err)
	}
	p.a.mu.Lock()
	_, hasStale := p.a.dlAuth["tok-stale"]
	p.a.mu.Unlock()
	if hasStale {
		t.Fatal("过期 token 应从表中摘除")
	}
	// 新鲜：照常取回
	if got, err := p.b.download("bench-a", "tok-fresh", "dump.bin"); err != nil || string(got) != "NVS-STALE" {
		t.Fatalf("新鲜 token 应放行: %q err=%v", got, err)
	}
}

// mesh 刷机完成（成功路径）后会话目录与字节账一并清理。
func TestFlashUploadDirRemovedAfterFlash(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "app.bin")
	if err := os.WriteFile(binPath, []byte("FW-IMAGE"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd == "flash" && len(req.Spec.Bins) == 1 {
			respond(ctl.Response{OK: true, Event: "flash-done"})
			return
		}
		respond(ctl.Response{OK: false, Error: "unexpected"})
	}, nil)
	if err := p.b.remoteFlash("bench-a", ctl.Request{Cmd: "flash", Pattern: "^dev$",
		Spec: flashSpecFor(binPath)}, func(ctl.Response) {}); err != nil {
		t.Fatalf("remoteFlash 失败: %v", err)
	}
	testutil.WaitFor(t, 2*time.Second, func() bool {
		entries, err := os.ReadDir(filepath.Join(p.rootA, ".flash-upload"))
		return err == nil && len(entries) == 0
	}, "刷机完成后 .flash-upload 会话目录应清理")
	p.a.mu.Lock()
	sids := len(p.a.upBytes)
	p.a.mu.Unlock()
	if sids != 0 {
		t.Fatalf("上传字节账应随会话清理: %d", sids)
	}
}

// 启动清扫：超 24h（mtime）的 .mesh-share/.flash-upload 目录删除，新近保留。
func TestStartupSweepArtifacts(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		dir := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	oldShare := mk(".mesh-share", "old1")
	oldUp := mk(".flash-upload", "mesh-old2")
	freshShare := mk(".mesh-share", "fresh3")
	past := time.Now().Add(-25 * time.Hour)
	for _, d := range []string{oldShare, oldUp} {
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatal(err)
		}
	}
	n, err := NewNode(Options{Name: "sweep", Key: "k", Root: root, NoUDP: true,
		Forward: func(ctl.Request, func(ctl.Response)) {}, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := os.Stat(oldShare); !os.IsNotExist(err) {
		t.Fatalf("陈旧 .mesh-share 目录应被清扫: %v", err)
	}
	if _, err := os.Stat(oldUp); !os.IsNotExist(err) {
		t.Fatalf("陈旧 .flash-upload 目录应被清扫: %v", err)
	}
	if _, err := os.Stat(freshShare); err != nil {
		t.Fatalf("新近目录不应被清扫: %v", err)
	}
}
