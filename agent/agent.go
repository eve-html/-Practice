package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Agent - сервис-агент на каждой ноде
type Agent struct {
	ID            string
	Host          string
	Port          int
	ControllerURL string
	Services      map[string]*ServiceProcess
	mu            sync.RWMutex
	ctx           context.Context
	cancel        context.CancelFunc
}

// ServiceProcess информация о процессе сервиса
type ServiceProcess struct {
	ID        string
	PID       int
	Port      int
	Cmd       *exec.Cmd
	StartedAt time.Time
	Status    string
	Config    string
}

// AgentCommand команда от контроллера
type AgentCommand struct {
	Action    string `json:"action"`
	ServiceID string `json:"service_id"`
	Config    string `json:"config"`
	Port      int    `json:"port"`
}

// NewAgent создает нового агента
func NewAgent(id, host string, port int, controllerURL string) *Agent {
	ctx, cancel := context.WithCancel(context.Background())

	return &Agent{
		ID:            id,
		Host:          host,
		Port:          port,
		ControllerURL: controllerURL,
		Services:      make(map[string]*ServiceProcess),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start запускает агента
func (a *Agent) Start() error {
	log.Printf("Agent %s starting...", a.ID)

	// Регистрация в контроллере
	if err := a.registerWithController(); err != nil {
		log.Printf("Warning: Failed to register with controller: %v", err)
	}

	// Запуск heartbeat
	go a.heartbeatLoop()

	// Запуск мониторинга процессов
	go a.monitorProcesses()

	log.Printf("Agent %s started on port %d", a.ID, a.Port)
	return nil
}

// Stop останавливает агента
func (a *Agent) Stop() {
	a.cancel()

	// Останавливаем все сервисы
	a.mu.Lock()
	for id, svc := range a.Services {
		log.Printf("Stopping service %s...", id)
		if svc.Cmd != nil && svc.Cmd.Process != nil {
			svc.Cmd.Process.Kill()
			svc.Cmd.Wait()
		}
	}
	a.Services = make(map[string]*ServiceProcess)
	a.mu.Unlock()

	log.Printf("Agent %s stopped", a.ID)
}

// registerWithController регистрирует агента в контроллере
func (a *Agent) registerWithController() error {
	data := map[string]interface{}{
		"id":   a.ID,
		"host": a.Host,
		"port": a.Port,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	url := a.ControllerURL + "/register"
	resp, err := http.Post(url, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller returned status: %d", resp.StatusCode)
	}

	log.Printf("Agent %s registered with controller", a.ID)
	return nil
}

// heartbeatLoop отправляет heartbeat контроллеру
func (a *Agent) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.sendHeartbeat()
		}
	}
}

// sendHeartbeat отправляет heartbeat
func (a *Agent) sendHeartbeat() {
	a.mu.RLock()
	services := make([]map[string]interface{}, 0, len(a.Services))
	for id, svc := range a.Services {
		services = append(services, map[string]interface{}{
			"id":     id,
			"port":   svc.Port,
			"status": svc.Status,
		})
	}
	a.mu.RUnlock()

	data := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"services":  services,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("Failed to marshal heartbeat: %v", err)
		return
	}

	url := fmt.Sprintf("%s/heartbeat/%s", a.ControllerURL, a.ID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonData))
	if err != nil {
		log.Printf("Failed to create heartbeat request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to send heartbeat: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Controller heartbeat response: %d", resp.StatusCode)
	}
}

// monitorProcesses мониторит запущенные процессы
func (a *Agent) monitorProcesses() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.checkProcesses()
		}
	}
}

// checkProcesses проверяет состояние процессов
func (a *Agent) checkProcesses() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for id, svc := range a.Services {
		if svc.Cmd == nil || svc.Cmd.Process == nil {
			svc.Status = "failed"
			log.Printf("Service %s: no process found", id)
			continue
		}

		if !a.isPortListening(svc.Port) {
			svc.Status = "failed"
			log.Printf("Service %s port %d is not listening", id, svc.Port)
			continue
		}

		process, err := os.FindProcess(svc.PID)
		if err != nil {
			svc.Status = "failed"
			log.Printf("Service %s process not found: %v", id, err)
			continue
		}

		if err := process.Signal(os.Signal(nil)); err != nil {
			svc.Status = "failed"
			log.Printf("Service %s is not running: %v", id, err)
		} else {
			svc.Status = "running"
		}
	}
}

// isPortListening проверяет, слушается ли порт
func (a *Agent) isPortListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// setupRoutes настраивает HTTP маршруты агента
func (a *Agent) setupRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/command", a.commandHandler)
	mux.HandleFunc("/status", a.statusHandler)
	mux.HandleFunc("/health", a.healthHandler)
	mux.HandleFunc("/service/stop/", a.stopServiceHandler)
	mux.HandleFunc("/stop-all", a.stopAllHandler)

	return mux
}

// commandHandler обрабатывает команды от контроллера
func (a *Agent) commandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cmd AgentCommand

	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("Received command: %+v", cmd)

	var err error

	switch cmd.Action {
	case "start":
		err = a.startService(cmd)
	case "stop":
		err = a.stopService(cmd.ServiceID)
	default:
		err = fmt.Errorf("unknown action: %s", cmd.Action)
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"status":     "ok",
		"action":     cmd.Action,
		"service_id": cmd.ServiceID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// startService запускает сервис (УПРОЩЕННАЯ ВЕРСИЯ)
func (a *Agent) startService(cmd AgentCommand) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.Services[cmd.ServiceID]; exists {
		return fmt.Errorf("service %s already exists", cmd.ServiceID)
	}

	if a.isPortListening(cmd.Port) {
		return fmt.Errorf("port %d is already in use", cmd.Port)
	}

	// ВСЕ файлы теперь в одной папке bin!
	// payload.exe и agent.exe в одной директории

	binaryName := "payload.exe"
	currentDir, _ := os.Getwd()
	binaryPath := filepath.Join(currentDir, binaryName)

	log.Printf("Starting service %s with binary: %s, port: %d",
		cmd.ServiceID, binaryPath, cmd.Port)

	// Простой конфиг файл рядом с payload.exe
	configPath := filepath.Join(currentDir, "config-"+cmd.ServiceID+".json")
	simpleConfig := `{"id": "` + cmd.ServiceID + `", "port": ` + fmt.Sprintf("%d", cmd.Port) + `}`

	if err := os.WriteFile(configPath, []byte(simpleConfig), 0644); err != nil {
		return fmt.Errorf("failed to write config: %v", err)
	}

	// Запускаем payload.exe
	proc := exec.Command(binaryPath,
		"--id", cmd.ServiceID,
		"--port", fmt.Sprintf("%d", cmd.Port),
		"--config", configPath,
	)

	// Для отладки
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr

	if err := proc.Start(); err != nil {
		return fmt.Errorf("failed to start process: %v", err)
	}

	time.Sleep(2 * time.Second) // Даем время запуститься

	if proc.Process == nil {
		return fmt.Errorf("process failed to start")
	}

	if !a.isPortListening(cmd.Port) {
		proc.Process.Kill()
		proc.Wait()
		return fmt.Errorf("service started but port %d is not listening", cmd.Port)
	}

	a.Services[cmd.ServiceID] = &ServiceProcess{
		ID:        cmd.ServiceID,
		PID:       proc.Process.Pid,
		Port:      cmd.Port,
		Cmd:       proc,
		StartedAt: time.Now(),
		Status:    "running",
		Config:    cmd.Config,
	}

	log.Printf("Service %s started successfully (PID: %d, Port: %d)",
		cmd.ServiceID, proc.Process.Pid, cmd.Port)

	return nil
}

// stopService останавливает сервис
func (a *Agent) stopService(serviceID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	svc, exists := a.Services[serviceID]
	if !exists {
		return fmt.Errorf("service %s not found", serviceID)
	}

	log.Printf("Stopping service %s (PID: %d)...", serviceID, svc.PID)

	if svc.Cmd != nil && svc.Cmd.Process != nil {
		if err := svc.Cmd.Process.Kill(); err != nil {
			log.Printf("Warning: failed to kill process: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := svc.Cmd.Process.Wait()
			done <- err
		}()

		select {
		case <-done:
			log.Printf("Service %s stopped", serviceID)
		case <-time.After(5 * time.Second):
			log.Printf("Service %s didn't stop gracefully", serviceID)
		}
	}

	delete(a.Services, serviceID)
	return nil
}

// stopAllHandler останавливает все сервисы
func (a *Agent) stopAllHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	count := 0
	for id, svc := range a.Services {
		log.Printf("Stopping service %s...", id)
		if svc.Cmd != nil && svc.Cmd.Process != nil {
			svc.Cmd.Process.Kill()
			svc.Cmd.Wait()
		}
		count++
	}

	a.Services = make(map[string]*ServiceProcess)

	response := map[string]interface{}{
		"status":        "ok",
		"stopped_count": count,
		"message":       fmt.Sprintf("Stopped %d services", count),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// statusHandler возвращает статус агента
func (a *Agent) statusHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	services := make([]map[string]interface{}, 0, len(a.Services))
	for id, svc := range a.Services {
		services = append(services, map[string]interface{}{
			"id":         id,
			"pid":        svc.PID,
			"port":       svc.Port,
			"status":     svc.Status,
			"started_at": svc.StartedAt.Format(time.RFC3339),
			"uptime":     time.Since(svc.StartedAt).String(),
		})
	}

	status := map[string]interface{}{
		"agent_id":  a.ID,
		"host":      a.Host,
		"port":      a.Port,
		"status":    "running",
		"services":  services,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// healthHandler health check агента
func (a *Agent) healthHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	serviceCount := len(a.Services)
	a.mu.RUnlock()

	healthStatus := "healthy"
	if serviceCount == 0 {
		healthStatus = "healthy_no_services"
	}

	response := map[string]interface{}{
		"status":        healthStatus,
		"agent_id":      a.ID,
		"service_count": serviceCount,
		"timestamp":     time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// stopServiceHandler останавливает сервис через HTTP
func (a *Agent) stopServiceHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	serviceID := strings.TrimPrefix(path, "/service/stop/")

	if serviceID == "" {
		http.Error(w, "Service ID required", http.StatusBadRequest)
		return
	}

	if err := a.stopService(serviceID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"status":     "stopped",
		"service_id": serviceID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
