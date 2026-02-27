package main

import (
	"io"
	"log"
	"os/exec"
)

func initializeAudioOutputStream() (io.WriteCloser, *exec.Cmd) {
	player := exec.Command("ffmpeg", "-f", "s16le", "-ar", "24000", "-ac", "1", "-i", "pipe:0", "-f", "audiotoolbox", "-")
	playerIn, err := player.StdinPipe()
	if err != nil {
		log.Println("failed to create player stdin pipe:", err)
		return nil, nil
	}
	if err := player.Start(); err != nil {
		log.Println("failed to start player:", err)
		return nil, nil
	}
	return playerIn, player
}
