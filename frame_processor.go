package main

import "context"

type FrameProcessor interface {
	Process(ctx context.Context, in <-chan Frame, out chan<- Frame)
}
