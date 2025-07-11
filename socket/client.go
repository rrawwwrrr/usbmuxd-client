package socket

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log2 "github.com/sirupsen/logrus"
)

var log = log2.WithField("component", "client")

// Tunnel описывает конфигурацию одного туннеля
type Tunnel struct {
	localAddr string // например: "127.0.0.1:7777" или "/var/run/usbmuxd"
	handshake string // ключ для сервера: "forward" или "usbmuxd"
}

// Переменные окружения
var (
	serverAddr   = os.Getenv("USBMUXD_HOST")
	serverPort   = os.Getenv("USBMUXD_PORT")
	serverSocket = os.Getenv("USBMUXD_SOCKET_ADDRESS")
)

// tunnels — список туннелей, которые нужно запустить
var tunnels = []Tunnel{
	{localAddr: serverSocket, handshake: "usbmuxd"},
	{localAddr: "127.0.0.1:7777", handshake: "forward"},
}

func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	opErr, ok := err.(*net.OpError)
	return ok && (opErr.Err.Error() == "use of closed network connection" || opErr.Err.Error() == "connection reset by peer")
}

func isConnectionOpen(conn net.Conn) (bool, error) {
	if conn == nil {
		return false, errors.New("соединение равно nil")
	}
	if _, err := conn.Write(nil); err != nil {
		return false, err
	}
	return true, nil
}

func startProxy(a, b net.Conn) {
	log.WithFields(log2.Fields{
		"from": a.RemoteAddr(),
		"to":   b.RemoteAddr(),
	}).Info("Начало проксирования")

	var wg sync.WaitGroup
	wg.Add(2)

	closeOnce := sync.OnceFunc(func() {
		a.Close()
		b.Close()
	})

	go func() {
		defer wg.Done()
		if ok, _ := isConnectionOpen(a); !ok {
			log.Debug("A уже закрыто, не запускаем A->B")
			return
		}
		_, err := io.Copy(b, a)
		if err != nil && !isClosedError(err) {
			log.WithError(err).WithFields(log2.Fields{
				"source": a.RemoteAddr(),
				"dest":   b.RemoteAddr(),
			}).Error("Ошибка A->B")
		}
		closeOnce()
	}()

	go func() {
		defer wg.Done()
		if ok, _ := isConnectionOpen(b); !ok {
			log.Debug("B уже закрыто, не запускаем B->A")
			return
		}
		_, err := io.Copy(a, b)
		if err != nil && !isClosedError(err) {
			log.WithError(err).WithFields(log2.Fields{
				"source": b.RemoteAddr(),
				"dest":   a.RemoteAddr(),
			}).Error("Ошибка B->A")
		}
		closeOnce()
	}()

	wg.Wait()
	log.Info("Проксирование завершено")
}

func connectToServer(handshake string) (net.Conn, error) {
	serverFullAddr := fmt.Sprintf("%s:%s", serverAddr, serverPort)
	conn, err := net.DialTimeout("tcp", serverFullAddr, 10*time.Second)
	if err != nil {
		log.WithError(err).WithField("server", serverFullAddr).Error("Ошибка подключения к серверу")
		return nil, err
	}

	// Отправляем handshake
	if _, err := conn.Write([]byte(handshake + "\n")); err != nil {
		log.WithError(err).Error("Ошибка отправки handshake")
		conn.Close()
		return nil, err
	}

	return conn, nil
}

// handleUnixSocket создаёт Unix-сокет и слушает на нём
func handleUnixSocket(t Tunnel) {
	socketPath := t.localAddr

	// Очищаем путь от старого сокета, если он есть
	os.Remove(socketPath)

	// Создаём директорию, если её нет
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		log.WithError(err).WithField("path", filepath.Dir(socketPath)).Fatal("Не удалось создать директорию для сокета")
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.WithError(err).WithField("socket", socketPath).Fatal("Не удалось создать Unix-сокет")
	}
	defer listener.Close()

	log.WithField("socket", socketPath).Info("Создан и слушается Unix-сокет")

	for {
		localConn, err := listener.Accept()
		if err != nil {
			log.WithError(err).Error("Ошибка принятия соединения на Unix-сокете")
			continue
		}

		log.WithField("client", localConn.RemoteAddr()).Info("Новое подключение к Unix-сокету")

		// Подключаемся к серверу
		serverConn, err := connectToServer(t.handshake)
		if err != nil {
			log.WithError(err).Error("Не удалось подключиться к серверу")
			localConn.Close()
			continue
		}

		// Запускаем прокси
		go startProxy(localConn, serverConn)
	}
}

// handleTCPListener создаёт TCP-слушателя и перенаправляет подключения
func handleTCPListener(t Tunnel) {
	tcpAddr := t.localAddr

	// Создаём TCP-слушателя
	listener, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		log.WithError(err).WithField("address", tcpAddr).Fatal("Не удалось создать TCP-слушателя")
	}
	defer listener.Close()

	log.WithField("address", tcpAddr).Info("Создан и слушается TCP-слушатель")

	for {
		localConn, err := listener.Accept()
		if err != nil {
			log.WithError(err).Error("Ошибка принятия соединения на TCP-порту")
			continue
		}

		log.WithField("client", localConn.RemoteAddr()).Info("Новое подключение к TCP-порту")

		// Подключаемся к серверу
		serverConn, err := connectToServer(t.handshake)
		if err != nil {
			log.WithError(err).Error("Не удалось подключиться к серверу")
			localConn.Close()
			continue
		}

		// Запускаем прокси
		go startProxy(localConn, serverConn)
	}
}

func runTunnel(t Tunnel) {
	log.WithFields(log2.Fields{
		"local":     t.localAddr,
		"handshake": t.handshake,
	}).Info("Запуск туннеля")

	// Если это Unix-сокет — создаём и слушаем
	if strings.HasPrefix(t.localAddr, "/") {
		handleUnixSocket(t)
		return
	}

	// Если это TCP-адрес — создаём TCP-слушателя
	if strings.Contains(t.localAddr, ":") {
		handleTCPListener(t)
		return
	}

	// Иначе — обычное TCP-подключение
	serverConn, err := connectToServer(t.handshake)
	if err != nil {
		log.WithError(err).Error("Не удалось подключиться к серверу")
		return
	}

	localConn, err := net.Dial("tcp", t.localAddr)
	if err != nil {
		log.WithError(err).WithField("local", t.localAddr).Error("Ошибка подключения к локальному ресурсу")
		serverConn.Close()
		return
	}

	startProxy(localConn, serverConn)
}

// Run запускает все туннели из списка
func Run() {
	if serverAddr == "" || serverPort == "" {
		log.Fatal("Переменные окружения USBMUXD_HOST и USBMUXD_PORT должны быть установлены")
	}

	var wg sync.WaitGroup

	for _, tunnel := range tunnels {
		wg.Add(1)
		go func(t Tunnel) {
			defer wg.Done()
			runTunnel(t)
		}(tunnel)
	}

	wg.Wait()
	log.Info("Все туннели завершили работу")
}
