package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/soheilhy/cmux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "ultramultiplexer/pb/pb"
)

type UltraMultiplexer struct {
	port       string
	listener   net.Listener
	mux        cmux.CMux
	httpServer *http.Server
	grpcServer *grpc.Server

	httpClient *http.Client
	grpcClient pb.UltraServiceClient
	grpcConn   *grpc.ClientConn

	mu          sync.RWMutex
	serverReady bool
	muxStarted  bool
}

type HTTPHandler struct {
	multiplexer *UltraMultiplexer
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		h.healthCheck(w, r)
	case "/proxy":
		h.proxyRequest(w, r)
	case "/grpc-call":
		h.callGRPC(w, r)
	default:
		h.defaultHandler(w, r)
	}
}

func (h *HTTPHandler) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"service":   "ultra-multiplexer",
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func (h *HTTPHandler) proxyRequest(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "target parameter required", http.StatusBadRequest)
		return
	}

	resp, err := h.multiplexer.httpClient.Get(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *HTTPHandler) callGRPC(w http.ResponseWriter, r *http.Request) {
	if !h.multiplexer.isGRPCClientReady() {
		http.Error(w, "gRPC client not ready", http.StatusServiceUnavailable)
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		name = "World"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reply, err := h.multiplexer.grpcClient.SayHello(ctx, &pb.HelloRequest{
		Name: name,
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("gRPC call failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"grpc_response": reply.Message,
	})
}

func (h *HTTPHandler) defaultHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Ultra Multiplexer HTTP Server",
		"method":  r.Method,
		"path":    r.URL.Path,
	})
}

type GRPCServer struct {
	pb.UnimplementedUltraServiceServer
	multiplexer *UltraMultiplexer
}

func (s *GRPCServer) SayHello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloReply, error) {
	message := fmt.Sprintf("Hello %s from Ultra Multiplexer!", req.Name)
	return &pb.HelloReply{Message: message}, nil
}

func (s *GRPCServer) ProcessData(ctx context.Context, req *pb.DataRequest) (*pb.DataReply, error) {
	processed := strings.ToUpper(req.Data)
	return &pb.DataReply{Processed: processed}, nil
}

func NewUltraMultiplexer(port string) *UltraMultiplexer {
	return &UltraMultiplexer{
		port: port,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		serverReady: false,
		muxStarted:  false,
	}
}

func (um *UltraMultiplexer) Initialize() error {
	listener, err := net.Listen("tcp", ":"+um.port)
	if err != nil {
		return fmt.Errorf("failed to create listener: %v", err)
	}
	um.listener = listener

	um.mux = cmux.New(listener)

	// ВАЖНО: Используем более надежные матчеры
	grpcListener := um.mux.MatchWithWriters(
		cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"),
	)
	httpListener := um.mux.Match(cmux.Any())

	httpHandler := &HTTPHandler{multiplexer: um}
	um.httpServer = &http.Server{
		Handler:      httpHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	um.grpcServer = grpc.NewServer()
	grpcServerImpl := &GRPCServer{multiplexer: um}
	pb.RegisterUltraServiceServer(um.grpcServer, grpcServerImpl)

	// Запускаем серверы
	go func() {
		log.Println("🌐 Starting HTTP server...")
		if err := um.httpServer.Serve(httpListener); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	go func() {
		log.Println("🔗 Starting gRPC server...")
		if err := um.grpcServer.Serve(grpcListener); err != nil {
			log.Printf("gRPC server error: %v", err)
		}
	}()

	return nil
}

func (um *UltraMultiplexer) startMux() {
	um.mu.Lock()
	if um.muxStarted {
		um.mu.Unlock()
		return
	}
	um.muxStarted = true
	um.mu.Unlock()

	go func() {
		log.Println("🚀 Starting cmux...")
		if err := um.mux.Serve(); err != nil {
			log.Printf("Mux serve error: %v", err)
		}
	}()
}

func (um *UltraMultiplexer) waitForServerReady() error {
	log.Println("⏳ Waiting for servers to be ready...")

	for attempts := 0; attempts < 20; attempts++ {
		// Проверяем готовность HTTP сервера
		httpReady := um.checkHTTPReady()
		if !httpReady {
			log.Printf("🔄 HTTP server not ready yet... (attempt %d/20)", attempts+1)
			time.Sleep(1 * time.Second)
			continue
		}

		// Проверяем готовность gRPC сервера
		grpcReady := um.checkGRPCReady()
		if !grpcReady {
			log.Printf("🔄 gRPC server not ready yet... (attempt %d/20)", attempts+1)
			time.Sleep(1 * time.Second)
			continue
		}

		log.Println("✅ Both servers are ready!")
		return nil
	}

	return fmt.Errorf("servers not ready after 20 attempts")
}

func (um *UltraMultiplexer) checkHTTPReady() bool {
	client := &http.Client{Timeout: 1 * time.Second}
	_, err := client.Get("http://localhost:" + um.port + "/health")
	return err == nil
}

func (um *UltraMultiplexer) checkGRPCReady() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "localhost:"+um.port,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())

	if err != nil {
		return false
	}

	conn.Close()
	return true
}

func (um *UltraMultiplexer) initGRPCClient() error {
	log.Println("🔌 Initializing gRPC client...")

	// Используем более длительный таймаут для подключения
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "localhost:"+um.port,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock())

	if err != nil {
		return fmt.Errorf("failed to connect gRPC client: %v", err)
	}

	um.grpcConn = conn
	um.grpcClient = pb.NewUltraServiceClient(conn)
	um.serverReady = true

	log.Println("✅ gRPC client successfully connected!")
	return nil
}

func (um *UltraMultiplexer) isGRPCClientReady() bool {
	um.mu.RLock()
	defer um.mu.RUnlock()
	return um.serverReady
}

func (um *UltraMultiplexer) Start() error {
	log.Printf("🚀 Ultra Multiplexer starting on port %s", um.port)

	// 1. Запускаем cmux
	um.startMux()

	// 2. Ждем готовности серверов
	if err := um.waitForServerReady(); err != nil {
		return fmt.Errorf("servers not ready: %v", err)
	}

	// 3. Инициализируем gRPC клиент
	if err := um.initGRPCClient(); err != nil {
		return fmt.Errorf("failed to initialize gRPC client: %v", err)
	}

	log.Printf("📡 HTTP endpoints: /health, /proxy, /grpc-call")
	log.Printf("🔗 gRPC services: SayHello, ProcessData")
	log.Printf("✅ Ultra Multiplexer is fully ready!")

	select {} // Блокируем основной поток
}

func (um *UltraMultiplexer) Stop() error {
	um.mu.Lock()
	defer um.mu.Unlock()

	if um.grpcConn != nil {
		um.grpcConn.Close()
	}

	if um.httpServer != nil {
		um.httpServer.Close()
	}

	if um.grpcServer != nil {
		um.grpcServer.Stop()
	}

	if um.listener != nil {
		um.listener.Close()
	}

	return nil
}

func main() {
	multiplexer := NewUltraMultiplexer("8080")

	if err := multiplexer.Initialize(); err != nil {
		log.Fatalf("Failed to initialize: %v", err)
	}

	if err := multiplexer.Start(); err != nil {
		log.Fatalf("Failed to start: %v", err)
	}
}
