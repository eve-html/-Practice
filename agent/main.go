package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Парсинг флагов
	id := flag.String("id", "node1", "Agent ID")
	host := flag.String("host", "0.0.0.0", "Agent host")
	port := flag.Int("port", 9990, "Agent port")
	controllerURL := flag.String("controller", "http://localhost:8888", "Controller URL")
	flag.Parse()

	// Создаем агента
	agent := NewAgent(*id, *host, *port, *controllerURL)

	// Настраиваем HTTP сервер
	addr := fmt.Sprintf("%s:%d", *host, *port)
	handler := agent.setupRoutes()

	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Запуск агента (heartbeat, мониторинг)
	go func() {
		if err := agent.Start(); err != nil {
			log.Printf("Agent background tasks error: %v", err)
		}
	}()

	// Запуск сервера
	go func() {
		log.Printf("Agent %s starting on %s", *id, addr)
		log.Printf("Controller: %s", *controllerURL)
		log.Printf("Features: S3 config support, health monitoring")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start agent server: %v", err)
		}
	}()

	// Ожидание сигнала остановки
	<-stop
	log.Printf("Shutting down agent %s...", *id)

	// Останавливаем агента
	agent.Stop()

	// Останавливаем HTTP сервер
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Error during agent shutdown: %v", err)
	}

	log.Printf("Agent %s stopped", *id)
}
