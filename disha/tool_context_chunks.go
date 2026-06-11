package disha

import "github.com/jaideep329/talk-go/voicepipelinecore"

func toolContextAdditionalData(message voicepipelinecore.Message) any {
	switch message.Role {
	case "assistant":
		if len(message.ToolCalls) == 0 {
			return nil
		}
		return map[string]any{"tool_calls": message.ToolCalls}
	case "tool":
		if message.ToolCallID == "" {
			return nil
		}
		return map[string]any{"tool_call_id": message.ToolCallID}
	default:
		return nil
	}
}
