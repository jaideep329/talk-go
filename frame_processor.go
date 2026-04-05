package main

type FrameProcessor interface {
	Process(in <-chan Frame, out chan<- Frame)
}
