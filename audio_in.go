package main

import (
	"io"
	"log"
	"os"
	"os/exec"
)

var audioInputStream io.ReadCloser

func initializeAudioCapture() {
	cmd := exec.Command("ffmpeg",
		"-f", "avfoundation",
		"-i", ":0",
		"-ac", "1",
		"-ar", "16000",
		"-f", "s16le",
		"-flush_packets", "1",
		"-",
	)
	f, _ := os.Create("ffmpeg.log")
	cmd.Stderr = f
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	audioInputStream = stdout
}
