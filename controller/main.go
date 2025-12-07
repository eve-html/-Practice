package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// S3ConfigStore - эмуляция S3 хранилища для конфигураций
type S3ConfigStore struct {
	configs map[string]string
}

func NewS3ConfigStore() *S3ConfigStore {
	return &S3ConfigStore{
		configs: map[string]string{
			"default":  `{"version": "1.0", "workload": "balanced", "health_check_interval": "5s"}`,
			"high-cpu": `{"cpu_limit": 80, "memory": "512MB", "workload": "cpu_intensive"}`,
			"high-mem": `{"cpu_limit": 50, "memory": "1GB", "workload": "memory_intensive"}`,
			"balanced": `{"cpu_limit": 70, "memory": "768MB", "workload": "balanced"}`,
			"minimal":  `{"cpu_limit": 30, "memory": "256MB", "workload": "minimal"}`,
		},
	}
}

func (s *S3ConfigStore) GetConfig(key string) (string, bool) {
	config, exists := s.configs[key]
	return config, exists
}

type Controller struct {
	mu              sync.RWMutex
	agents          map[string]*AgentInfo
	services        map[string]*ServiceInfo
	desiredReplicas int
	lastPortIndex   int
	serviceCounter  int
	configStore     *S3ConfigStore
	requestCount    int                  // Счетчик запросов для автоскейлинга
	failedServices  map[string]time.Time // Для отслеживания упавших сервисов
}

type AgentInfo struct {
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	Port     int       `json:"port"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen"`
	Services []string  `json:"services"`
}

type ServiceInfo struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	Port      int       `json:"port"`
	Config    string    `json:"config"`
}

func main() {
	port := flag.Int("port", 8888, "Controller port")
	replicas := flag.Int("replicas", 0, "Default replicas")
	flag.Parse()

	ctrl := &Controller{
		agents:          make(map[string]*AgentInfo),
		services:        make(map[string]*ServiceInfo),
		desiredReplicas: *replicas,
		lastPortIndex:   0,
		serviceCounter:  0,
		configStore:     NewS3ConfigStore(),
		requestCount:    0,
		failedServices:  make(map[string]time.Time),
	}

	// Мониторинг агентов (offline detection)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			ctrl.mu.Lock()
			for id, agent := range ctrl.agents {
				if time.Since(agent.LastSeen) > 35*time.Second {
					agent.Status = "offline"
					log.Printf("Agent %s marked as offline", id)
				}
			}
			ctrl.mu.Unlock()
		}
	}()

	// Health checking сервисов
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		for range ticker.C {
			ctrl.checkServicesHealth()
		}
	}()

	// Автоматический scaler (по desired_replicas и по нагрузке)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			ctrl.mu.Lock()

			// 1. Автоскейлинг по нагрузке
			currentLoad := ctrl.requestCount
			currentServices := len(ctrl.services)

			// Логика автоскейлинга по нагрузке
			if currentLoad > 20 && currentServices < 10 { // Высокая нагрузка
				needed := 2
				if ctrl.desiredReplicas < currentServices+needed {
					ctrl.desiredReplicas = currentServices + needed
					log.Printf("Auto-scaling UP due to high load (%d requests)", currentLoad)
				}
			} else if currentLoad < 5 && currentServices > 1 { // Низкая нагрузка
				needed := 1
				if ctrl.desiredReplicas > currentServices-needed {
					ctrl.desiredReplicas = currentServices - needed
					log.Printf("Auto-scaling DOWN due to low load (%d requests)", currentLoad)
				}
			}

			// Сбрасываем счетчик запросов
			ctrl.requestCount = 0

			// 2. Стандартный автоскейлинг по desired_replicas
			current := len(ctrl.services)
			needed := ctrl.desiredReplicas - current

			if needed > 0 {
				log.Printf("Auto-scaling: need %d more services", needed)
				ctrl.scaleUp(needed)
			} else if needed < 0 {
				// Удаляем лишние сервисы
				toRemove := -needed
				ctrl.removeServices(toRemove)
			}

			ctrl.mu.Unlock()
		}
	}()

	// Endpoint для эмуляции нагрузки
	http.HandleFunc("/api/load", func(w http.ResponseWriter, r *http.Request) {
		ctrl.mu.Lock()
		ctrl.requestCount++
		loadCount := ctrl.requestCount
		ctrl.mu.Unlock()

		response := map[string]interface{}{
			"status":       "request_processed",
			"request_id":   time.Now().UnixNano(),
			"current_load": loadCount,
			"timestamp":    time.Now().Format(time.RFC3339),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		ctrl.mu.RLock()
		defer ctrl.mu.RUnlock()

		agents := make([]*AgentInfo, 0, len(ctrl.agents))
		for _, agent := range ctrl.agents {
			// Гарантируем что Services всегда массив, а не nil
			if agent.Services == nil {
				agent.Services = []string{}
			}
			agents = append(agents, agent)
		}

		services := make([]*ServiceInfo, 0, len(ctrl.services))
		for _, svc := range ctrl.services {
			services = append(services, svc)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"timestamp":        time.Now().Format(time.RFC3339),
			"desired_replicas": ctrl.desiredReplicas,
			"running_replicas": len(ctrl.services),
			"current_load":     ctrl.requestCount,
			"agents":           agents,
			"services":         services,
			"config_source":    "S3 (emulated)",
			"auto_scaling":     "enabled",
			"health_checking":  "enabled",
		})
	})

	http.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID   string `json:"id"`
			Host string `json:"host"`
			Port int    `json:"port"`
		}

		json.NewDecoder(r.Body).Decode(&req)

		ctrl.mu.Lock()
		ctrl.agents[req.ID] = &AgentInfo{
			ID:       req.ID,
			Host:     req.Host,
			Port:     req.Port,
			Status:   "online",
			LastSeen: time.Now(),
			Services: []string{}, // Всегда инициализируем как пустой массив
		}
		ctrl.mu.Unlock()

		log.Printf("Agent registered: %s (configs will be loaded from S3)", req.ID)
		w.Write([]byte(`{"status": "registered", "config_source": "S3"}`))
	})

	http.HandleFunc("/heartbeat/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/heartbeat/")

		var req struct {
			Services []string `json:"services"`
		}

		json.NewDecoder(r.Body).Decode(&req)

		ctrl.mu.Lock()
		if agent, exists := ctrl.agents[agentID]; exists {
			agent.LastSeen = time.Now()
			agent.Status = "online"
			// Очищаем пустые строки при обновлении
			var cleanedServices []string
			for _, s := range req.Services {
				if s != "" {
					cleanedServices = append(cleanedServices, s)
				}
			}
			agent.Services = cleanedServices
		}
		ctrl.mu.Unlock()

		w.Write([]byte(`{"status": "ok"}`))
	})

	http.HandleFunc("/deploy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Replicas int    `json:"replicas"`
			Config   string `json:"config,omitempty"` // Конфиг из S3
		}

		json.NewDecoder(r.Body).Decode(&req)

		if req.Replicas < 1 {
			req.Replicas = 3
		}

		ctrl.mu.Lock()
		ctrl.desiredReplicas = req.Replicas

		// Удаляем старые сервисы
		for id := range ctrl.services {
			delete(ctrl.services, id)
		}

		// Очищаем список сервисов у агентов
		for _, agent := range ctrl.agents {
			agent.Services = []string{}
		}

		// Сбрасываем счетчики
		ctrl.lastPortIndex = 0
		ctrl.serviceCounter = 0

		// Создаем новые сервисы
		ctrl.scaleUp(req.Replicas)

		ctrl.mu.Unlock()

		log.Printf("Deployed %d services (configs from S3)", req.Replicas)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "deployed",
			"desired_replicas": req.Replicas,
			"config_source":    "S3",
		})
	})

	http.HandleFunc("/scale/up", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count int `json:"count"`
		}

		json.NewDecoder(r.Body).Decode(&req)

		if req.Count < 1 {
			req.Count = 1
		}

		ctrl.mu.Lock()
		ctrl.desiredReplicas += req.Count

		// Сразу создаем сервисы
		ctrl.scaleUp(req.Count)

		ctrl.mu.Unlock()

		log.Printf("Scaled up by %d", req.Count)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "scaling_up",
			"desired_replicas": ctrl.desiredReplicas,
			"added":            req.Count,
		})
	})

	http.HandleFunc("/scale/down", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count int `json:"count"`
		}

		json.NewDecoder(r.Body).Decode(&req)

		if req.Count < 1 {
			req.Count = 1
		}

		ctrl.mu.Lock()
		// Не позволяем desiredReplicas уйти ниже 0
		newDesired := ctrl.desiredReplicas - req.Count
		if newDesired < 0 {
			newDesired = 0
			req.Count = ctrl.desiredReplicas
		}
		ctrl.desiredReplicas = newDesired

		// Удаляем сервисы
		removedCount := ctrl.removeServices(req.Count)

		ctrl.mu.Unlock()

		log.Printf("Scaled down by %d (removed %d services)", req.Count, removedCount)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "scaling_down",
			"desired_replicas": ctrl.desiredReplicas,
			"requested":        req.Count,
			"removed":          removedCount,
		})
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := "healthy"
		ctrl.mu.RLock()
		if len(ctrl.services) == 0 {
			status = "no_services"
		}
		ctrl.mu.RUnlock()

		response := map[string]interface{}{
			"status":        status,
			"timestamp":     time.Now().Format(time.RFC3339),
			"config_source": "S3",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Endpoint для получения информации о конфигах в S3
	http.HandleFunc("/api/configs", func(w http.ResponseWriter, r *http.Request) {
		configs := []map[string]string{
			{"name": "default", "description": "Balanced workload"},
			{"name": "high-cpu", "description": "CPU intensive workload"},
			{"name": "high-mem", "description": "Memory intensive workload"},
			{"name": "balanced", "description": "Optimized for balance"},
			{"name": "minimal", "description": "Minimal resource usage"},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"configs":       configs,
			"source":        "S3 (emulated)",
			"total_configs": len(configs),
		})
	})

	log.Printf("Enhanced controller starting on :%d", *port)
	log.Printf("Features: S3 configs, auto-scaling, health checking")
	http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
}

// checkServicesHealth проверяет здоровье всех сервисов
func (c *Controller) checkServicesHealth() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	servicesToRestart := make(map[string]*ServiceInfo)

	for id, svc := range c.services {
		if svc.Status == "failed" {
			// Проверяем, не пытались ли мы уже недавно перезапустить
			if lastTry, exists := c.failedServices[id]; exists {
				if now.Sub(lastTry) < 30*time.Second {
					continue // Еще не прошло 30 секунд с последней попытки
				}
			}
			servicesToRestart[id] = svc
			continue
		}

		// Проверяем health сервиса
		if !c.checkServiceHealth(svc.Port) {
			log.Printf("Service %s is unhealthy, marking as failed", id)
			svc.Status = "failed"
			c.failedServices[id] = now
			servicesToRestart[id] = svc
		}
	}

	// Перезапускаем упавшие сервисы
	for id, svc := range servicesToRestart {
		go c.restartService(id, svc)
	}
}

// checkServiceHealth проверяет доступность сервиса
func (c *Controller) checkServiceHealth(port int) bool {
	url := fmt.Sprintf("http://localhost:%d/health", port)
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// restartService перезапускает упавший сервис
func (c *Controller) restartService(oldID string, oldSvc *ServiceInfo) {
	time.Sleep(5 * time.Second) // Задержка перед перезапуском

	c.mu.Lock()
	defer c.mu.Unlock()

	// Проверяем, существует ли еще сервис
	if _, exists := c.services[oldID]; !exists {
		return // Уже удален
	}

	// Создаем новый ID
	newID := fmt.Sprintf("svc-restarted-%d-%s", c.serviceCounter, oldSvc.AgentID)
	c.serviceCounter++

	// Создаем новую запись
	c.services[newID] = &ServiceInfo{
		ID:        newID,
		AgentID:   oldSvc.AgentID,
		Status:    "running",
		StartedAt: time.Now(),
		Port:      oldSvc.Port,
		Config:    oldSvc.Config,
	}

	// Удаляем старую
	delete(c.services, oldID)
	delete(c.failedServices, oldID)

	// Обновляем у агента (с очисткой пустых строк)
	if agent, exists := c.agents[oldSvc.AgentID]; exists {
		// Гарантируем что Services не nil
		if agent.Services == nil {
			agent.Services = []string{}
		}
		var cleanedServices []string
		for _, s := range agent.Services {
			if s != "" && s != oldID {
				cleanedServices = append(cleanedServices, s)
			}
		}
		cleanedServices = append(cleanedServices, newID)
		agent.Services = cleanedServices
	}

	log.Printf("Restarting service %s -> %s on agent %s", oldID, newID, oldSvc.AgentID)

	// Отправляем команду перезапуска
	go c.sendToAgentWithRetry(oldSvc.AgentID, newID, oldSvc.Port, oldSvc.Config, 3)
}

// scaleUp создает указанное количество сервисов
func (c *Controller) scaleUp(count int) {
	log.Printf("Creating %d services with S3 configs", count)

	for i := 0; i < count; i++ {
		// Выбираем агента
		var agents []string
		for id, agent := range c.agents {
			if agent.Status == "online" {
				agents = append(agents, id)
			}
		}

		if len(agents) == 0 {
			log.Println("No online agents available")
			return
		}

		// Round-robin распределение
		agentID := agents[i%len(agents)]

		// УНИКАЛЬНЫЙ ID на основе счетчика
		serviceID := fmt.Sprintf("svc-%d-%s", c.serviceCounter, agentID)
		c.serviceCounter++

		// Уникальный порт (начинаем с 10000)
		port := 10000 + c.lastPortIndex
		c.lastPortIndex++

		// Выбираем конфиг из S3 на основе логики
		configKey := "default"
		switch c.serviceCounter % 5 {
		case 0:
			configKey = "high-cpu"
		case 1:
			configKey = "high-mem"
		case 2:
			configKey = "balanced"
		case 3:
			configKey = "minimal"
		}

		config, exists := c.configStore.GetConfig(configKey)
		if !exists {
			config = c.configStore.configs["default"]
		}

		// Создаем запись о сервисе
		c.services[serviceID] = &ServiceInfo{
			ID:        serviceID,
			AgentID:   agentID,
			Status:    "running",
			StartedAt: time.Now(),
			Port:      port,
			Config:    config,
		}

		// Добавляем к агенту (ИСПРАВЛЕННЫЙ КОД - гарантируем не nil и очищаем пустые строки)
		if agent, exists := c.agents[agentID]; exists {
			// Гарантируем что Services не nil
			if agent.Services == nil {
				agent.Services = []string{}
			}
			// Удаляем пустые строки если есть
			var cleanedServices []string
			for _, s := range agent.Services {
				if s != "" {
					cleanedServices = append(cleanedServices, s)
				}
			}
			cleanedServices = append(cleanedServices, serviceID)
			agent.Services = cleanedServices
		}

		// Отправляем команду агенту
		go c.sendToAgentWithRetry(agentID, serviceID, port, config, 3)

		log.Printf("Created service %s on agent %s (port: %d, config: %s)",
			serviceID, agentID, port, configKey)
	}
}

// removeServices удаляет указанное количество сервисов
func (c *Controller) removeServices(count int) int {
	removedCount := 0

	// Собираем ID сервисов для удаления
	servicesToRemove := make([]string, 0)
	for id := range c.services {
		if len(servicesToRemove) >= count {
			break
		}
		servicesToRemove = append(servicesToRemove, id)
	}

	// Удаляем сервисы
	for _, id := range servicesToRemove {
		svc, exists := c.services[id]
		if !exists {
			continue
		}

		// Удаляем из контроллера
		delete(c.services, id)
		delete(c.failedServices, id)

		// Удаляем из агента (ИСПРАВЛЕННЫЙ КОД - гарантируем не nil и очищаем пустые строки)
		if agent, exists := c.agents[svc.AgentID]; exists {
			// Гарантируем что Services не nil
			if agent.Services == nil {
				agent.Services = []string{}
			}
			var cleanedServices []string
			for _, s := range agent.Services {
				if s != "" && s != id {
					cleanedServices = append(cleanedServices, s)
				}
			}
			agent.Services = cleanedServices
		}

		// Отправляем команду остановки агенту
		go c.stopService(svc.AgentID, id)

		removedCount++
	}

	return removedCount
}

// sendToAgentWithRetry отправляет команду агенту с повторными попытками
func (c *Controller) sendToAgentWithRetry(agentID, serviceID string, port int, config string, maxRetries int) {
	c.mu.RLock()
	agent, exists := c.agents[agentID]
	c.mu.RUnlock()

	if !exists {
		log.Printf("Agent %s not found for service %s", agentID, serviceID)
		return
	}

	cmd := map[string]interface{}{
		"action":     "start",
		"service_id": serviceID,
		"port":       port,
		"config":     config,
	}

	url := fmt.Sprintf("http://localhost:%d/command", agent.Port)
	data, _ := json.Marshal(cmd)

	for retry := 0; retry < maxRetries; retry++ {
		log.Printf("Sending command to agent %s (attempt %d/%d)", agentID, retry+1, maxRetries)

		resp, err := http.Post(url, "application/json", bytes.NewReader(data))
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			log.Printf("Command to agent %s succeeded", agentID)
			return
		}

		if resp != nil {
			resp.Body.Close()
		}

		if retry < maxRetries-1 {
			waitTime := 2 * time.Second * time.Duration(retry+1)
			log.Printf("Retry %d for agent %s in %v", retry+1, agentID, waitTime)
			time.Sleep(waitTime)
		}
	}

	log.Printf("Failed to send command to agent %s after %d attempts", agentID, maxRetries)
}

// stopService отправляет команду остановки агенту
func (c *Controller) stopService(agentID, serviceID string) {
	c.mu.RLock()
	agent, exists := c.agents[agentID]
	c.mu.RUnlock()

	if !exists {
		return
	}

	cmd := map[string]interface{}{
		"action":     "stop",
		"service_id": serviceID,
	}

	url := fmt.Sprintf("http://localhost:%d/command", agent.Port)
	data, _ := json.Marshal(cmd)

	log.Printf("Sending stop command to agent %s for service %s", agentID, serviceID)

	// Игнорируем ошибки для демо
	http.Post(url, "application/json", bytes.NewReader(data))
}
