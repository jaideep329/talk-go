package main

import (
	"log"
	"os"
	"strings"
)

func main() {
	loadEnv(".env")
	appLog, _ := os.Create("app.log")
	log.SetOutput(appLog)
	initializeAudioCapture()
	initializeSonioxWebsocket()
	initializeTTSWebsocket()
	defer sttConn.Close()
	done := make(chan struct{})
	go readSTTWebsocketLoop(done)
	writeAudioInToSTTWebsocket(done)

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
