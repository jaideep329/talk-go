package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	loadEnv(".env")
	appLog, _ := os.Create("app.log")
	log.SetOutput(io.MultiWriter(os.Stderr, appLog))
	joinRoom()
	http.HandleFunc("/getToken", handleGetToken)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "livekit-client.html")
	})
	fmt.Println("HTTP server listening on :3000")
	if err := http.ListenAndServe(":3000", nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("failed to read .env:", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			os.Setenv(parts[0], parts[1])
		}
	}
}
