package socket

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"usbmuxd-client/crypt"

	log "github.com/sirupsen/logrus"
)

// Tunnel описывает конфигурацию одного туннеля
type Tunnel struct {
	localAddr  string // например: "127.0.0.1:7777" или "/var/run/usbmuxd"
	handshake  string // ключ для сервера: "forward" или "usbmuxd"
	sync       bool   // если true, добавляем флаг sync в handshake
	persistent bool   // если true — поддерживает persistent mux mode
}

// Переменные окружения
var (
	serverAddr    = os.Getenv("USBMUXD_HOST")
	serverPort    = os.Getenv("USBMUXD_PORT")
	serverSocket  = os.Getenv("USBMUXD_SOCKET")
	device        = os.Getenv("DEVICE")
	deviceType    = os.Getenv("DEVICE_TYPE")
	tunnelMode = func() string {
		if v := os.Getenv("TUNNEL_MODE"); v != "" {
			return v
		}
		return "persistent"
	}() // "persistent" по умолчанию, можно переопределить через TUNNEL_MODE=transient
)

// tunnelsIos — список туннелей для iOS
var tunnelsIos = []Tunnel{
	{localAddr: serverSocket, handshake: device + " usbmux", persistent: true},
	{localAddr: "127.0.0.1:7777", handshake: device + " wda", persistent: true},
}

// tunnelsAndroid — список туннелей для Android
var tunnelsAndroid = []Tunnel{
	{localAddr: "127.0.0.1:5037", handshake: device + " adb", persistent: true},
	{localAddr: "127.0.0.1:0", handshake: device + " appium", sync: true}, // динамические порты (8200-8299)
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
	// listeners: порт → активный net.Listener (для динамических портов Appium)
	listeners := make(map[int]net.Listener)

	closeAll := func() {
		for p, l := range listeners {
			l.Close()
			delete(listeners, p)
		}
	}

	for {
		conn, err := connectToServer(t.handshake, true)
		if err != nil {
			log.Warn("Не удалось подключиться к sync-каналу, retry...")
			time.Sleep(2 * time.Second)
			continue
		}

		log.Info("Sync-канал установлен")

		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			cmd, arg, _ := strings.Cut(line, " ")

			switch cmd {
			case "PORT_READY":
				port := parsePort(arg)
				if port == 0 {
					// Обратная совместимость: PORT_READY без номера → используем дефолтный порт из handshake
					log.Info("PORT_READY (legacy) → порт из handshake")
					if _, ok := listeners[0]; !ok {
						l := startListener(t)
						if l != nil {
							listeners[0] = l
						}
					}
					continue
				}
				if _, ok := listeners[port]; ok {
					continue // уже открыт
				}
				log.Infof("PORT_READY %d → открываем localhost:%d", port, port)
				l := startListenerOnPort(t, port)
				if l != nil {
					listeners[port] = l
				}

			case "PORT_DOWN":
				port := parsePort(arg)
				if port == 0 {
					// Legacy: PORT_DOWN без номера — закрываем всё
					log.Warn("PORT_DOWN (legacy) → закрываем все порты")
					closeAll()
					continue
				}
				if l, ok := listeners[port]; ok {
					log.Warnf("PORT_DOWN %d → закрываем localhost:%d", port, port)
					l.Close()
					delete(listeners, port)
				}
			}
		}

		log.Warn("Sync-соединение потеряно")
		closeAll()
		conn.Close()
		time.Sleep(2 * time.Second)
	}
}

func parsePort(s string) int {
	if s == "" {
		return 0
	}
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return p
}

// startListenerOnPort запускает listener на конкретном порту и проксирует через hub-server.
func startListenerOnPort(t Tunnel, port int) net.Listener {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Errorf("Не удалось открыть %s: %v", addr, err)
		return nil
	}
	// Формируем handshake с конкретным портом: "<serial> appium port:N"
	handshake := fmt.Sprintf("%s port:%d", t.handshake, port)
	go func() {
		defer l.Close()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				serverConn, err := connectToServer(handshake, false)
				if err != nil {
					log.WithError(err).Errorf("Ошибка подключения к серверу для порта %d", port)
					return
				}
				defer serverConn.Close()
				startProxy(c, serverConn)
			}(conn)
		}
	}()
	return l
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

	if t.persistent && tunnelMode == "persistent" {
		runPersistentADBTunnel(t)
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
