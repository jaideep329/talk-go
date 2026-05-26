package disha

import (
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const callEventRequestTimeout = 10 * time.Second

func registerSalesCallEvents(operations CallOperations) voicepipelinecore.CallEvents {
	return CallEventsForOperations(operations)
}
