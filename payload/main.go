package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// Парсинг флагов
	serviceID := flag.String("id", "", "Service ID (required)")
	port := flag.Int("port", 10000, "Service port")
	configPath := flag.String("config", "", "Config file path (optional)")
	flag.Parse()

	if *serviceID == "" {
		// Пробуем получить из переменной окружения
		*serviceID = os.Getenv("SERVICE_ID")
		if *serviceID == "" {
			log.Fatal("Service ID is required")
		}
	}

	if *port == 0 {
		portStr := os.Getenv("PORT")
		if portStr != "" {
			fmt.Sscanf(portStr, "%d", port)
		}
		if *port == 0 {
			*port = 10000
		}
	}

	// Создаем и запускаем сервис
	service := &PayloadService{
		ID:         *serviceID,
		Port:       *port,
		ConfigPath: *configPath, // Сохраняем путь к конфигу
	}

	log.Printf("Payload service %s starting on port %d", *serviceID, *port)
	if *configPath != "" {
		log.Printf("Using config file: %s", *configPath)
	}
	service.Start()
}

// PayloadService - сервис полезной нагрузки
type PayloadService struct {
	ID         string
	Port       int
	ConfigPath string
	StartTime  time.Time
}

// Start запускает сервис
func (ps *PayloadService) Start() {
	ps.StartTime = time.Now()

	// Загружаем конфигурацию если указан путь
	if ps.ConfigPath != "" {
		if err := ps.loadConfig(); err != nil {
			log.Printf("Warning: failed to load config: %v", err)
		}
	}

	mux := http.NewServeMux()

	// Health endpoint для проверки контроллером
	mux.HandleFunc("/health", ps.healthHandler)

	// Work endpoint для эмутации полезной нагрузки
	mux.HandleFunc("/work", ps.workHandler)

	// Info endpoint для получения информации о сервисе
	mux.HandleFunc("/info", ps.infoHandler)

	// Metrics endpoint для метрик
	mux.HandleFunc("/metrics", ps.metricsHandler)

	// Config endpoint для просмотра конфигурации
	mux.HandleFunc("/config", ps.configHandler)

	// Старт HTTP сервера
	addr := fmt.Sprintf(":%d", ps.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Printf("Service %s listening on %s", ps.ID, addr)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start service: %v", err)
	}
}

// loadConfig загружает конфигурацию из файла
func (ps *PayloadService) loadConfig() error {
	data, err := os.ReadFile(ps.ConfigPath)
	if err != nil {
		return err
	}

	var config struct {
		Name      string `json:"name"`
		WorkDelay int    `json:"work_delay_ms"`
		LogLevel  string `json:"log_level"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}

	log.Printf("Loaded config: name=%s, work_delay=%dms, log_level=%s",
		config.Name, config.WorkDelay, config.LogLevel)

	return nil
}

// healthHandler проверяет здоровье сервиса
func (ps *PayloadService) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service_id": ps.ID,
		"status":     "healthy",
		"uptime":     time.Since(ps.StartTime).String(),
		"timestamp":  time.Now().Format(time.RFC3339),
		"port":       ps.Port,
	})
}

// workHandler эмулирует полезную работу
func (ps *PayloadService) workHandler(w http.ResponseWriter, r *http.Request) {
	// Имитация обработки запроса
	start := time.Now()

	// Эмутация работы (50-150ms)
	workDuration := 50 + time.Duration(time.Now().UnixNano()%100)*time.Millisecond
	time.Sleep(workDuration)

	// Генерация ответа
	response := map[string]interface{}{
		"service_id": ps.ID,
		"status":     "work_completed",
		"work_time":  time.Since(start).String(),
		"result":     "success",
		"data":       "Sample work result",
		"timestamp":  time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// infoHandler возвращает информацию о сервисе
func (ps *PayloadService) infoHandler(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()

	info := map[string]interface{}{
		"id":          ps.ID,
		"port":        ps.Port,
		"started_at":  ps.StartTime.Format(time.RFC3339),
		"uptime":      time.Since(ps.StartTime).String(),
		"hostname":    hostname,
		"pid":         os.Getpid(),
		"config_path": ps.ConfigPath,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// metricsHandler возвращает метрики сервиса
func (ps *PayloadService) metricsHandler(w http.ResponseWriter, r *http.Request) {
	metrics := map[string]interface{}{
		"service_id":      ps.ID,
		"uptime_seconds":  time.Since(ps.StartTime).Seconds(),
		"status":          "running",
		"timestamp":       time.Now().Unix(),
		"requests_served": 0, // Можно добавить счетчик запросов
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

// configHandler возвращает информацию о конфигурации
func (ps *PayloadService) configHandler(w http.ResponseWriter, r *http.Request) {
	configInfo := map[string]interface{}{
		"service_id":  ps.ID,
		"config_path": ps.ConfigPath,
		"has_config":  ps.ConfigPath != "",
		"timestamp":   time.Now().Format(time.RFC3339),
	}

	if ps.ConfigPath != "" {
		if data, err := os.ReadFile(ps.ConfigPath); err == nil {
			configInfo["config_content"] = string(data)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(configInfo)
}
