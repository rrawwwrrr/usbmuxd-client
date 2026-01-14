package socket

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"usbmuxd-client/crypt"

	log "github.com/sirupsen/logrus"
)

// Tunnel описывает конфигурацию одного туннеля
type Tunnel struct {
	localAddr string // например: "127.0.0.1:7777" или "/var/run/usbmuxd"
	handshake string // ключ для сервера: "forward" или "usbmuxd"
	sync      bool   // если true, добавляем флаг sync в handshake
}

// Переменные окружения
var (
	serverAddr   = os.Getenv("USBMUXD_HOST")
	serverPort   = os.Getenv("USBMUXD_PORT")
	serverSocket = os.Getenv("USBMUXD_SOCKET")
	device       = os.Getenv("DEVICE")
	deviceType   = os.Getenv("DEVICE_TYPE")
)

// tunnelsIos — список туннелей для iOS
var tunnelsIos = []Tunnel{
	{localAddr: serverSocket, handshake: device + " usbmux"},
	{localAddr: "127.0.0.1:7777", handshake: device + " wda"},
}

// tunnelsAndroid — список туннелей для Android
var tunnelsAndroid = []Tunnel{
	{localAddr: "127.0.0.1:5037", handshake: device + " adb"},
	{localAddr: "127.0.0.1:8200", handshake: device + " appium", sync: true},
}

func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "broken pipe") ||
		err == io.EOF
}

func startProxy(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	closeOnce := &sync.Once{}
	closeBoth := func() {
		closeOnce.Do(func() {
			a.Close()
			b.Close()
		})
	}

	go func() {
		defer wg.Done()
		defer closeBoth()
		io.Copy(b, a)
	}()

	go func() {
		defer wg.Done()
		defer closeBoth()
		io.Copy(a, b)
	}()

	wg.Wait()
}

// connectToServer устанавливает соединение с сервером
func connectToServer(handshake string, sync bool) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%s", serverAddr, serverPort)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}

	full := handshake
	if sync {
		full += " sync"
	}

	encrypted, err := crypt.EncryptHandshake(full)
	if err != nil {
		conn.Close()
		return nil, err
	}

	if _, err := conn.Write([]byte(encrypted + "\n")); err != nil {
		conn.Close()
		return nil, err
	}

	// ⬇⬇⬇ ВОТ КЛЮЧЕВОЕ МЕСТО ⬇⬇⬇
	if !sync {
		buf := make([]byte, 16)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(buf)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			conn.Close()
			return nil, err
		}

		resp := strings.TrimSpace(string(buf[:n]))
		if resp != "OK" {
			conn.Close()
			return nil, fmt.Errorf("server error: %s", resp)
		}
	}

	return conn, nil
}

func startListener(t Tunnel) net.Listener {
	l, err := net.Listen("tcp", t.localAddr)
	if err != nil {
		log.WithError(err).Error("Не удалось создать listener")
		return nil
	}

	log.WithField("local", t.localAddr).Info("Локальный порт открыт")

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()

				serverConn, err := connectToServer(t.handshake, false)
				if err != nil {
					log.WithError(err).Error("Ошибка подключения к серверу")
					return
				}
				defer serverConn.Close()

				startProxy(c, serverConn)
			}(conn)
		}
	}()

	return l
}

func runSyncTunnel(t Tunnel) {
	for {
		conn, err := connectToServer(t.handshake, true)
		if err != nil {
			log.Warn("Не удалось подключиться к sync-каналу, retry...")
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("Sync-канал установлен")

		scanner := bufio.NewScanner(conn)
		var listener net.Listener

		for scanner.Scan() {
			msg := strings.TrimSpace(scanner.Text())

			switch msg {
			case "PORT_READY":
				if listener == nil {
					log.Info("PORT_READY → открываем порт")
					listener = startListener(t)
				}

			case "PORT_DOWN":
				if listener != nil {
					log.Warn("PORT_DOWN → закрываем порт")
					listener.Close()
					listener = nil
				}
			}
		}

		log.Warn("Sync-соединение потеряно")
		if listener != nil {
			listener.Close()
		}
		conn.Close()
		time.Sleep(2 * time.Second)
	}
}

// Unix socket
func handleUnixSocket(t Tunnel) {
	os.Remove(t.localAddr)
	os.MkdirAll(filepath.Dir(t.localAddr), 0755)

	l, err := net.Listen("unix", t.localAddr)
	if err != nil {
		log.Fatal(err)
	}

	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}

		go func(conn net.Conn) {
			defer conn.Close()

			serverConn, err := connectToServer(t.handshake, false)
			if err != nil {
				return
			}
			defer serverConn.Close()

			startProxy(conn, serverConn)
		}(c)
	}
}

func runTunnel(t Tunnel) {
	if t.sync {
		runSyncTunnel(t)
		return
	}

	// Если это Unix-сокет — создаём и слушаем
	if strings.HasPrefix(t.localAddr, "/") {
		handleUnixSocket(t)
		return
	}

	startListener(t)
}

func Run() {
	if serverAddr == "" || serverPort == "" || device == "" {
		log.Fatal("Не заданы обязательные ENV")
	}

	var tunnels []Tunnel

	if deviceType == "ios" || deviceType == "" {
		tunnels = tunnelsIos
		log.Info("Используем туннели для iOS")
	} else if deviceType == "android" {
		tunnels = tunnelsAndroid
		log.Info("Используем туннели для Android")
	} else {
		log.Fatalf("Неизвестный тип устройства: %s", deviceType)
	}

	for _, t := range tunnels {
		go runTunnel(t)
	}

	select {} // держим процесс живым
}
