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

// ConfigFromS3 структура конфига из S3
type ConfigFromS3 struct {
	Version        string `json:"version"`
	Workload       string `json:"workload"`
	CPULimit       int    `json:"cpu_limit,omitempty"`
	Memory         string `json:"memory,omitempty"`
	HealthInterval string `json:"health_check_interval,omitempty"`
	Message        string `json:"message,omitempty"`
}

func main() {
	// Парсинг аргументов
	id := flag.String("id", "test", "Service ID")
	port := flag.Int("port", 8080, "Service port")
	configFile := flag.String("config", "", "Config file path (from S3)")
	s3Config := flag.Bool("s3-config", false, "Config is from S3")
	flag.Parse()

	log.Printf("Payload service %s starting on port %d", *id, *port)

	var s3ConfigData ConfigFromS3
	var configSource string

	if *configFile != "" {
		configData, err := os.ReadFile(*configFile)
		if err == nil {
			if *s3Config {
				configSource = "S3"
				if err := json.Unmarshal(configData, &s3ConfigData); err == nil {
					log.Printf("Loaded S3 config: %+v", s3ConfigData)
				}
			} else {
				configSource = "local"
				log.Printf("Loaded local config: %s", string(configData))
			}
		} else {
			log.Printf("Warning: failed to load config: %v", err)
		}
	}

	// HTTP обработчики
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"status":      "healthy",
			"service":     *id,
			"port":        *port,
			"timestamp":   time.Now().Format(time.RFC3339),
			"uptime":      time.Since(startTime).String(),
			"config_from": configSource,
		}

		// Добавляем информацию о S3 конфиге если есть
		if configSource == "S3" {
			response["s3_config"] = s3ConfigData
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"service_id":    *id,
			"config_file":   *configFile,
			"config_from":   configSource,
			"has_s3_config": configSource == "S3",
		}

		if configSource == "S3" {
			response["s3_config"] = s3ConfigData
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		message := "Payload service is running"
		if configSource == "S3" {
			message = fmt.Sprintf("Payload service is running with S3 config: %s", s3ConfigData.Workload)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service": *id,
			"message": message,
			"config":  configSource,
			"uptime":  time.Since(startTime).String(),
		})
	})

	// Запуск сервера
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Service %s listening on %s", *id, addr)
	log.Printf("Config source: %s", configSource)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

var startTime = time.Now()
