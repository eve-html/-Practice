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

type Controller struct {
	mu              sync.RWMutex
	agents          map[string]*AgentInfo
	services        map[string]*ServiceInfo
	desiredReplicas int
	lastPortIndex   int
	serviceCounter  int // УНИКАЛЬНЫЙ СЧЕТЧИК для ID сервисов
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
		serviceCounter:  0, // Инициализируем счетчик
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

	// Автоматический scaler
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			ctrl.mu.Lock()

			// Считаем сколько сервисов должно быть
			current := len(ctrl.services)
			needed := ctrl.desiredReplicas - current

			if needed > 0 {
				log.Printf("Auto-scaling: need %d more services", needed)
				ctrl.scaleUp(needed)
			}

			ctrl.mu.Unlock()
		}
	}()

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		ctrl.mu.RLock()
		defer ctrl.mu.RUnlock()

		agents := make([]*AgentInfo, 0, len(ctrl.agents))
		for _, agent := range ctrl.agents {
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
			"agents":           agents,
			"services":         services,
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
			Services: []string{},
		}
		ctrl.mu.Unlock()

		log.Printf("Agent registered: %s", req.ID)
		w.Write([]byte(`{"status": "registered"}`))
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
			agent.Services = req.Services
		}
		ctrl.mu.Unlock()

		w.Write([]byte(`{"status": "ok"}`))
	})

	http.HandleFunc("/deploy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Replicas int `json:"replicas"`
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

		log.Printf("Deployed %d services", req.Replicas)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "deployed",
			"desired_replicas": req.Replicas,
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
		toRemove := req.Count
		removedCount := 0

		// Собираем ID сервисов для удаления
		servicesToRemove := make([]string, 0)
		for id := range ctrl.services {
			if len(servicesToRemove) >= toRemove {
				break
			}
			servicesToRemove = append(servicesToRemove, id)
		}

		// Удаляем сервисы
		for _, id := range servicesToRemove {
			svc, exists := ctrl.services[id]
			if !exists {
				continue
			}

			// Удаляем из контроллера
			delete(ctrl.services, id)

			// Удаляем из агента
			if agent, exists := ctrl.agents[svc.AgentID]; exists {
				for i, s := range agent.Services {
					if s == id {
						agent.Services = append(agent.Services[:i], agent.Services[i+1:]...)
						break
					}
				}
			}

			// Отправляем команду остановки агенту
			go ctrl.stopService(svc.AgentID, id)

			removedCount++
		}

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
		w.Write([]byte(`{"status": "healthy"}`))
	})

	log.Printf("Controller starting on :%d", *port)
	http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
}

// scaleUp создает указанное количество сервисов
func (c *Controller) scaleUp(count int) {
	log.Printf("Creating %d services, serviceCounter: %d", count, c.serviceCounter)

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

		// Создаем запись о сервисе
		c.services[serviceID] = &ServiceInfo{
			ID:        serviceID,
			AgentID:   agentID,
			Status:    "running",
			StartedAt: time.Now(),
			Port:      port,
			Config:    "{}",
		}

		// Добавляем к агенту
		if agent, exists := c.agents[agentID]; exists {
			agent.Services = append(agent.Services, serviceID)
		}

		// Отправляем команду агенту
		go c.sendToAgent(agentID, serviceID, port)

		log.Printf("Created service %s on agent %s (port: %d)", serviceID, agentID, port)
	}
}

// sendToAgent отправляет команду запуска агенту
func (c *Controller) sendToAgent(agentID, serviceID string, port int) {
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
		"config":     "{}",
	}

	url := fmt.Sprintf("http://localhost:%d/command", agent.Port)
	data, _ := json.Marshal(cmd)

	log.Printf("Sending start command to agent %s at %s", agentID, url)

	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("Failed to send command to agent %s: %v", agentID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Agent %s returned status: %d", agentID, resp.StatusCode)
	}
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
