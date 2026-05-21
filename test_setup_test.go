package main

// init runs once for the test binary. It redirects the STT and TTS
// dial URLs to a definitely-unreachable loopback so background connect
// goroutines fail fast (connection refused) instead of actually dialing
// Soniox/Cartesia. The retry loop keeps going until the test's
// taskCtx.Ctx is cancelled at the end of the test.
func init() {
	sttDialURL = "ws://127.0.0.1:1/test-no-soniox"
	ttsDialURL = "ws://127.0.0.1:1/test-no-cartesia?key="
}
