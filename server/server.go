package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WsConfig struct {
	Addr           string
	Path           string
	AllowedOrigins []string

	HandshakeTimeout time.Duration
	PingInterval     time.Duration
	PongWait         time.Duration
	WriteTimeout     time.Duration

	MaxReadMessageSize int
	ReadBufferSize     int
	WriteBufferSize    int
	EnableCompression  bool
}

func NewWsConfig(addr, path string, allowedOrigins []string) *WsConfig {
	return &WsConfig{
		Addr:           addr,
		Path:           path,
		AllowedOrigins: allowedOrigins,

		HandshakeTimeout: 10 * time.Second,
		PingInterval:     30 * time.Second,
		PongWait:         60 * time.Second,
		WriteTimeout:     10 * time.Second,

		MaxReadMessageSize: 10 * 1024 * 1024,

		ReadBufferSize:    256 * 1024,
		WriteBufferSize:   256 * 1024,
		EnableCompression: false,
	}
}

type WsCallback struct {
	Started      func()
	Stopped      func()
	OnConnect    func()
	OnDisconnect func(err error)
	OnMessage    func(msg []byte)
	OnError      func(err error)
}

type Server struct {
	config     *WsConfig
	upgrader   websocket.Upgrader
	conn       *websocket.Conn
	mu         sync.RWMutex
	writeMu    sync.Mutex
	closed     bool
	callbacks  *WsCallback
	logger     *log.Logger
	httpServer *http.Server
	ctx        context.Context
	cancel     context.CancelFunc

	startOnce    sync.Once
	shutdownOnce sync.Once
}

func NewServer(config *WsConfig, callback *WsCallback, logger *log.Logger) *Server {
	if callback == nil {
		callback = &WsCallback{}
	}
	if logger == nil {
		logger = log.New(os.Stdout, "[ws-server] ", log.LstdFlags|log.Llongfile)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:    config,
		logger:    logger,
		callbacks: callback,
		ctx:       ctx,
		cancel:    cancel,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if len(config.AllowedOrigins) == 1 && config.AllowedOrigins[0] == "*" {
					return true
				}
				return slices.Contains(config.AllowedOrigins, origin)
			},
			HandshakeTimeout:  config.HandshakeTimeout,
			ReadBufferSize:    config.ReadBufferSize,
			WriteBufferSize:   config.WriteBufferSize,
			EnableCompression: config.EnableCompression,
		},
	}
}

func (s *Server) OnMessage(handler func(msg []byte)) {
	s.callbacks.OnMessage = handler
}

func (s *Server) OnStarted(handler func()) {
	s.callbacks.Started = handler
}

func (s *Server) OnStopped(handler func()) {
	s.callbacks.Stopped = handler
}

func (s *Server) OnConnect(handler func()) {
	s.callbacks.OnConnect = handler
}

func (s *Server) OnDisconnect(handler func(err error)) {
	s.callbacks.OnDisconnect = handler
}

func (s *Server) OnError(handler func(err error)) {
	s.callbacks.OnError = handler
}

func (s *Server) Start() error {
	var startErr error

	s.startOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc(s.config.Path, s.handleWS)

		s.httpServer = &http.Server{
			Addr:         s.config.Addr,
			Handler:      mux,
			ReadTimeout:  s.config.PongWait,
			WriteTimeout: s.config.WriteTimeout,
		}

		s.logger.Printf("WebSocket server running at ws://localhost%s%s", s.config.Addr, s.config.Path)

		if s.callbacks.Started != nil {
			s.callbacks.Started()
		}

		startErr = s.httpServer.ListenAndServe()
	})

	return startErr
}

func (s *Server) Send(msg interface{}) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.conn == nil {
		return fmt.Errorf("no active WebSocket connection")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.conn.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout))
	if err := s.conn.WriteJSON(msg); err != nil {
		s.logger.Printf("Write error: %v", err)
		return err
	}
	return nil
}

func (s *Server) ShutdownConn() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil {
		_ = s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down normally"))
		s.logger.Println("Closing WebSocket connection...")
		_ = s.conn.Close()
		s.conn = nil
		if s.callbacks.OnDisconnect != nil {
			s.callbacks.OnDisconnect(fmt.Errorf("connection closed"))
		}
	}
}

func (s *Server) ShutdownServer() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.logger.Println("Shutting down HTTP server...")
		_ = s.httpServer.Shutdown(ctx)
		s.httpServer = nil
	}

	if s.callbacks.Stopped != nil {
		s.callbacks.Stopped()
	}
}

func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() {
		s.cancel()
		s.ShutdownConn()
		s.ShutdownServer()
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Println("WebSocket upgrade failed:", r.RemoteAddr, err)
		if s.callbacks.OnError != nil {
			s.callbacks.OnError(err)
		}
		http.Error(w, "WebSocket upgrade failed", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.conn != nil {
		s.logger.Println("A client is already connected — rejecting new connection.")
		conn.Close()
		s.mu.Unlock()
		return
	}
	s.conn = conn
	s.mu.Unlock()

	s.logger.Println("Client connected")

	if s.callbacks.OnConnect != nil {
		s.callbacks.OnConnect()
	}

	defer func() {
		s.ShutdownConn()
	}()

	conn.SetReadLimit(int64(s.config.MaxReadMessageSize))
	conn.SetReadDeadline(time.Now().Add(s.config.PongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(s.config.PongWait))
		return nil
	})

	go func(ctx context.Context) {
		ticker := time.NewTicker(s.config.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.mu.RLock()
				c := s.conn
				s.mu.RUnlock()

				if c == nil {
					return
				}

				c.SetWriteDeadline(time.Now().Add(s.config.WriteTimeout))
				if err := c.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(s.config.PingInterval)); err != nil {
					s.logger.Printf("Ping error: %v", err)
					return
				}
			}
		}
	}(s.ctx)

	for {
		s.mu.RLock()
		c := s.conn
		s.mu.RUnlock()

		if c == nil {
			break
		}

		_, msg, err := c.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				s.logger.Printf("Unexpected disconnect: %v", err)
			} else {
				s.logger.Printf("Client disconnected normally: %v", err)
			}
			break
		}

		if s.callbacks.OnMessage != nil {
			s.callbacks.OnMessage(msg)
		}
	}
}
