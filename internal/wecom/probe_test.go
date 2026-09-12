package wecom

// 临时诊断探针 (排查长连接 EOF / 出口中间盒), 默认跳过。
//
// 在被怀疑的主机上执行 (需要能读到 .env 里的同一套凭证):
//
//	WECOM_PROBE_LIVE=1 go test ./internal/wecom/ -run TestProbeLive -v -timeout 120s
//
// 判读:
//   - 看到 FRAME #1 errcode=0 errmsg=ok 且连接存活 >60s → 代码/凭证/本机网络均正常
//   - 有 errcode=0 但随后收到 close/disconnected_event → 另有实例用同一 bot_id 在抢连接
//   - 一直 i/o timeout / EOF, 拿不到 ack → 本机出口中间盒在拦 WebSocket 数据帧
//     再看 tls ok 行的 issuer: 非 DigiCert/GlobalSign 等公认可信 CA = MITM 设备
//
// 注意: 探针会用真实 bot_id 建连, 按企微规则会踢掉同 bot 的旧连接, 排查期间使用。

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestProbeLive(t *testing.T) {
	if os.Getenv("WECOM_PROBE_LIVE") != "1" {
		t.Skip("set WECOM_PROBE_LIVE=1 to run the live wecom probe")
	}

	botID := os.Getenv("WECOM_BOT_ID")
	botSecret := os.Getenv("WECOM_BOT_SECRET")
	if botID == "" || botSecret == "" {
		t.Skip("WECOM_BOT_ID / WECOM_BOT_SECRET env required (or load via `go test` after godotenv)")
	}

	cfg := Config{BotID: botID, BotSecret: botSecret, WSURL: "wss://openws.work.weixin.qq.com"}
	w := New(cfg)

	u, _ := url.Parse(cfg.WSURL)
	host := u.Host + ":443"
	t.Logf("dialing %s ...", host)
	rawConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()
	t.Logf("tcp ok: %s -> %s", rawConn.LocalAddr(), rawConn.RemoteAddr())

	tlsConn := tls.Client(rawConn, &tls.Config{ServerName: u.Host, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("tls: %v", err)
	}
	st := tlsConn.ConnectionState()
	issuer := "(unknown)"
	if len(st.PeerCertificates) > 0 {
		issuer = st.PeerCertificates[0].Issuer.String()
	}
	t.Logf("tls ok: issuer=%s", issuer)

	var wsConn io.ReadWriteCloser = tlsConn
	if _, err := w.wsHandshake(wsConn, u.Host, "/"); err != nil {
		t.Fatalf("ws handshake: %v", err)
	}
	t.Logf("ws upgrade 101 ok")

	w.mu.Lock()
	w.conn = wsConn
	w.connected = true
	w.mu.Unlock()

	if err := w.subscribe(); err != nil {
		t.Fatalf("subscribe write: %v", err)
	}
	t0 := time.Now()
	t.Logf("subscribe frame sent at %s", t0.Format("15:04:05.000"))

	// 认证成功后 30s ping (对齐官方 SDK)
	stopPing := make(chan struct{})
	go func() {
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-tk.C:
				if err := w.ping(); err != nil {
					t.Logf("ping err: %v", err)
					return
				}
				t.Logf("ping sent at %s", time.Now().Format("15:04:05.000"))
			}
		}
	}()
	defer close(stopPing)

	// 整体观测 90s, 只有一个总 deadline
	deadline := time.Now().Add(90 * time.Second)
	_ = wsConn.(interface{ SetDeadline(time.Time) error }).SetDeadline(deadline)

	frameNo := 0
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(wsConn, hdr[:]); err != nil {
			t.Logf("[%s, +%v] READ ERR after %d frames: %v",
				time.Now().Format("15:04:05.000"), time.Since(t0).Truncate(time.Second), frameNo, err)
			return
		}
		opcode := hdr[0] & 0x0F
		masked := hdr[1]&0x80 != 0
		plen := int(hdr[1] & 0x7F)
		if plen == 126 {
			var ext [2]byte
			io.ReadFull(wsConn, ext[:])
			plen = int(ext[0])<<8 | int(ext[1])
		} else if plen == 127 {
			var ext [8]byte
			io.ReadFull(wsConn, ext[:])
			for _, b := range ext {
				plen = plen<<8 | int(b)
			}
		}
		var mk [4]byte
		if masked {
			io.ReadFull(wsConn, mk[:])
		}
		buf := make([]byte, plen)
		io.ReadFull(wsConn, buf)
		if masked {
			for i := range buf {
				buf[i] ^= mk[i%4]
			}
		}
		frameNo++
		t.Logf("[%s, +%v] FRAME #%d opcode=0x%x plen=%d payload=%s",
			time.Now().Format("15:04:05.000"), time.Since(t0).Truncate(time.Second),
			frameNo, opcode, plen, truncateForLog(string(buf), 500))

		if opcode == 0x8 {
			t.Logf("server CLOSE frame: %s", fmt.Sprintf("%x", buf))
			return
		}
		if opcode == 0x1 {
			var f map[string]any
			if json.Unmarshal(buf, &f) == nil {
				if body, _ := f["body"].(map[string]any); body != nil {
					if ev, _ := body["event"].(map[string]any); ev != nil {
						t.Logf("*** eventtype=%v (disconnected_event = 被同 bot 的另一个连接踢掉)", ev["eventtype"])
					}
				}
			}
		}
	}
}
