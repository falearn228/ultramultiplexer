## Основная концепция

Ультра мультиплексор - это **единый сервис**, который может:
- **Принимать** HTTP/1.1 и gRPC запросы на одном порту
- **Отправлять** HTTP и gRPC запросы как клиент
- **Мультиплексировать** трафик между разными протоколами

## Архитектура системы

### 1. **Основные компоненты**

```
┌─────────────────────────────────────────┐
│            Ultra Multiplexer            │
├─────────────────────────────────────────┤
│  TCP Listener (port 8080)              │
│  ┌─────────────────────────────────────┐ │
│  │            CMux                     │ │
│  │  ┌─────────────┐  ┌─────────────┐  │ │
│  │  │ HTTP Server │  │ gRPC Server │  │ │
│  │  └─────────────┘  └─────────────┘  │ │
│  └─────────────────────────────────────┘ │
│                                         │
│  ┌─────────────┐  ┌─────────────┐      │
│  │ HTTP Client │  │ gRPC Client │      │
│  └─────────────┘  └─────────────┘      │
└─────────────────────────────────────────┘
```

### 2. **CMux - сердце системы**

**CMux** (Connection Multiplexer) - это библиотека, которая анализирует входящие TCP соединения и **маршрутизирует** их к правильному обработчику на основе содержимого пакетов.

```go
// Создание матчеров для разных протоколов
grpcListener := um.mux.MatchWithWriters(
    cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"),
)
httpListener := um.mux.Match(cmux.Any())
```

## Принцип работы мультиплексирования

### 1. **Анализ входящих соединений**

Когда клиент подключается к порту 8080, CMux анализирует **первые байты** соединения:

```
Входящий запрос → CMux анализирует заголовки
│
├─ HTTP/1.1 GET /health → HTTP Server
├─ gRPC (content-type: application/grpc) → gRPC Server
└─ WebSocket, HTTP/2 → HTTP Server (fallback)
```

### 2. **Матчеры протоколов**

CMux использует **матчеры** для определения типа трафика:

- **gRPC матчер**: Ищет HTTP/2 с заголовком `content-type: application/grpc`
- **HTTP матчер**: Обрабатывает все остальные соединения

### 3. **Маршрутизация трафика**

```go
// gRPC запрос
grpcurl -plaintext localhost:8080 pb.UltraService/SayHello
    ↓
CMux → gRPC Server → GRPCServer.SayHello()

// HTTP запрос  
curl http://localhost:8080/health
    ↓
CMux → HTTP Server → HTTPHandler.healthCheck()
```

## Последовательность инициализации

### 1. **Создание базовых компонентов**

```go
// 1. Создаем TCP listener
listener, err := net.Listen("tcp", ":8080")

// 2. Создаем CMux поверх listener
mux := cmux.New(listener)

// 3. Создаем отдельные listeners для каждого протокола
grpcListener := mux.MatchWithWriters(...)
httpListener := mux.Match(cmux.Any())
```

### 2. **Запуск серверов**

```go
// 4. Запускаем HTTP и gRPC серверы в горутинах
go httpServer.Serve(httpListener)
go grpcServer.Serve(grpcListener)

// 5. Запускаем CMux (начинает принимать соединения)
go mux.Serve()
```

### 3. **Инициализация клиентов**

```go
// 6. Ждем готовности серверов
waitForServerReady()

// 7. Создаем gRPC и HTTP клиенты
grpcClient := pb.NewUltraServiceClient(conn)
httpClient := &http.Client{}
```

## Двунаправленность (клиент + сервер)

### **Серверная часть**

```go
// HTTP сервер
func (h *HTTPHandler) callGRPC(w http.ResponseWriter, r *http.Request) {
    // Обрабатываем HTTP запрос
    // Можем внутри вызвать gRPC клиент
    reply, err := h.multiplexer.grpcClient.SayHello(ctx, req)
}

// gRPC сервер
func (s *GRPCServer) ProcessData(ctx context.Context, req *pb.DataRequest) {
    // Обрабатываем gRPC запрос
    // Можем внутри вызвать HTTP клиент
    resp, err := s.multiplexer.httpClient.Get("https://api.example.com")
}
```

### **Клиентская часть**

```go
// HTTP клиент
httpClient.Get("http://localhost:8080/health")

// gRPC клиент  
grpcClient.SayHello(ctx, &pb.HelloRequest{Name: "test"})
```

## Практический пример работы

### **Сценарий 1: HTTP → gRPC**

```
1. curl "http://localhost:8080/grpc-call?name=TestUser"
2. HTTP Server получает запрос
3. HTTPHandler.callGRPC() использует встроенный gRPC клиент
4. gRPC клиент делает вызов к локальному gRPC серверу
5. gRPC Server обрабатывает запрос
6. Ответ возвращается через HTTP
```

### **Сценарий 2: Прямой gRPC**

```
1. grpcurl -plaintext localhost:8080 pb.UltraService/SayHello
2. CMux определяет gRPC трафик
3. Направляет на gRPC Server
4. GRPCServer.SayHello() обрабатывает запрос
5. Возвращает protobuf ответ
```

## Преимущества архитектуры

### 1. **Единый порт**
- Не нужно помнить разные порты для разных протоколов
- Упрощает конфигурацию firewall и load balancer

### 2. **Эффективность**
- CMux работает на уровне TCP, минимальные накладные расходы
- Каждый протокол обрабатывается нативно

### 3. **Гибкость**
- Можно легко добавить новые протоколы (WebSocket, HTTP/2)
- Серверы могут взаимодействовать друг с другом

### 4. **Масштабируемость**
- Горутины обеспечивают concurrent обработку
- Каждый протокол может иметь свои настройки таймаутов

## Потенциальные проблемы и решения

### **Проблема с инициализацией**
```go
// Неправильно: пытаемся подключиться до запуска сервера
conn, err := grpc.Dial("localhost:8080") // Сервер еще не готов

// Правильно: ждем готовности сервера
waitForServerReady()
conn, err := grpc.Dial("localhost:8080")
```

### **Проблема с матчерами**
```go
// Неправильно: HTTP матчер может перехватить gRPC
httpListener := mux.Match(cmux.HTTP1Fast())

// Правильно: gRPC матчер должен быть первым
grpcListener := mux.MatchWithWriters(...)
httpListener := mux.Match(cmux.Any()) // fallback
```

Такая архитектура идеально подходит для **микросервисов**, **API Gateway** или **универсальных прокси**, где нужна максимальная гибкость в обработке разных типов запросов на одном порту.
