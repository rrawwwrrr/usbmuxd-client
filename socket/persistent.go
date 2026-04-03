package socket

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	muxMsgOpen  byte = 0x01
	muxMsgData  byte = 0x02
	muxMsgClose byte = 0x03
	muxMsgACK   byte = 0x04
	muxMsgErr   byte = 0x05
)

type muxFrame struct {
	id      uint32
	typ     byte
	payload []byte
}

func writeMuxFrame(w io.Writer, f muxFrame) error {
	header := make([]byte, 9)
	binary.BigEndian.PutUint32(header[0:4], f.id)
	header[4] = f.typ
	binary.BigEndian.PutUint32(header[5:9], uint32(len(f.payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(f.payload) > 0 {
		_, err := w.Write(f.payload)
		return err
	}
	return nil
}

func readMuxFrame(r io.Reader) (muxFrame, error) {
	header := make([]byte, 9)
	if _, err := io.ReadFull(r, header); err != nil {
		return muxFrame{}, err
	}
	id := binary.BigEndian.Uint32(header[0:4])
	typ := header[4]
	length := binary.BigEndian.Uint32(header[5:9])
	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return muxFrame{}, err
		}
	}
	return muxFrame{id: id, typ: typ, payload: payload}, nil
}

type muxStream struct {
	id     uint32
	ackCh  chan bool   // true=ACK, false=ERR
	dataCh chan []byte // incoming DATA from server; closed on CLOSE
	once   sync.Once
}

func (s *muxStream) closeCh() {
	s.once.Do(func() { close(s.dataCh) })
}

type muxSession struct {
	conn    net.Conn
	writeMu sync.Mutex
	streams sync.Map // uint32 → *muxStream
	nextID  atomic.Uint32
	done    chan struct{}
}

func newMuxSession(conn net.Conn) *muxSession {
	ms := &muxSession{
		conn: conn,
		done: make(chan struct{}),
	}
	go ms.readLoop()
	return ms
}

func (ms *muxSession) write(f muxFrame) error {
	ms.writeMu.Lock()
	defer ms.writeMu.Unlock()
	return writeMuxFrame(ms.conn, f)
}

func (ms *muxSession) readLoop() {
	defer close(ms.done)
	defer ms.conn.Close()
	for {
		f, err := readMuxFrame(ms.conn)
		if err != nil {
			ms.streams.Range(func(k, v interface{}) bool {
				v.(*muxStream).closeCh()
				return true
			})
			return
		}
		v, ok := ms.streams.Load(f.id)
		if !ok {
			continue
		}
		s := v.(*muxStream)
		switch f.typ {
		case muxMsgACK:
			select {
			case s.ackCh <- true:
			default:
			}
		case muxMsgErr:
			select {
			case s.ackCh <- false:
			default:
			}
		case muxMsgData:
			select {
			case s.dataCh <- f.payload:
			case <-ms.done:
			}
		case muxMsgClose:
			s.closeCh()
			ms.streams.Delete(f.id)
		}
	}
}

func (ms *muxSession) openStream() (*muxStream, error) {
	id := ms.nextID.Add(1)
	s := &muxStream{
		id:     id,
		ackCh:  make(chan bool, 1),
		dataCh: make(chan []byte, 256),
	}
	ms.streams.Store(id, s)

	if err := ms.write(muxFrame{id: id, typ: muxMsgOpen}); err != nil {
		ms.streams.Delete(id)
		return nil, err
	}

	select {
	case ok := <-s.ackCh:
		if !ok {
			ms.streams.Delete(id)
			return nil, fmt.Errorf("stream %d rejected by server", id)
		}
		return s, nil
	case <-time.After(5 * time.Second):
		ms.streams.Delete(id)
		return nil, fmt.Errorf("stream %d open timeout", id)
	case <-ms.done:
		return nil, fmt.Errorf("session closed")
	}
}

func (ms *muxSession) proxyADB(adbConn net.Conn) {
	s, err := ms.openStream()
	if err != nil {
		log.WithError(err).Error("[mux] не удалось открыть stream")
		return
	}

	id := s.id
	var wg sync.WaitGroup
	wg.Add(2)

	// ADB client → server (DATA frames)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := adbConn.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				if werr := ms.write(muxFrame{id: id, typ: muxMsgData, payload: data}); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		ms.write(muxFrame{id: id, typ: muxMsgClose})
		ms.streams.Delete(id)
	}()

	// server DATA frames → ADB client
	go func() {
		defer wg.Done()
		for {
			select {
			case data, ok := <-s.dataCh:
				if !ok {
					adbConn.Close()
					return
				}
				if _, err := adbConn.Write(data); err != nil {
					return
				}
			case <-ms.done:
				adbConn.Close()
				return
			}
		}
	}()

	wg.Wait()
}

// runPersistentADBTunnel держит одно постоянное соединение к hub-server
// и мультиплексирует все входящие ADB-соединения поверх него.
func runPersistentADBTunnel(t Tunnel) {
	ln, err := net.Listen("tcp", t.localAddr)
	if err != nil {
		log.WithError(err).Fatal("[mux] не удалось открыть порт")
	}
	log.WithField("local", t.localAddr).Info("Локальный порт открыт (persistent mode)")

	var (
		sessionMu sync.Mutex
		session   *muxSession
	)

	getSession := func() *muxSession {
		sessionMu.Lock()
		defer sessionMu.Unlock()

		if session != nil {
			select {
			case <-session.done:
				session = nil
			default:
				return session
			}
		}

		conn, err := connectToServer(t.handshake+" persistent", false)
		if err != nil {
			log.WithError(err).Warn("[mux] не удалось подключиться к серверу")
			return nil
		}
		session = newMuxSession(conn)
		log.Info("[mux] persistent-сессия установлена")
		return session
	}

	for {
		adbConn, err := ln.Accept()
		if err != nil {
			return
		}

		go func(c net.Conn) {
			defer c.Close()
			for i := 0; i < 3; i++ {
				s := getSession()
				if s == nil {
					time.Sleep(500 * time.Millisecond)
					continue
				}
				s.proxyADB(c)
				return
			}
			log.Error("[mux] нет доступной сессии")
		}(adbConn)
	}
}
