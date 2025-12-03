package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	port := flag.Int("port", 8888, "Controller port")
	flag.Parse()

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		log.Println("GET /status")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"timestamp":        time.Now().Format(time.RFC3339),
			"desired_replicas": 0,
			"running_replicas": 0,
			"agents":           []string{},
			"services":         []string{},
		})
		log.Println("Response sent")
	})

	http.HandleFunc("/deploy", func(w http.ResponseWriter, r *http.Request) {
		log.Println("POST /deploy")

		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Replicas int `json:"replicas"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "deployed",
			"desired_replicas": req.Replicas,
		})
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status": "healthy"}`))
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Debug controller starting on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
