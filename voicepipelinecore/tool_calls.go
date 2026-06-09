package voicepipelinecore

// ToolCallAccumulator merges streamed OpenAI tool-call deltas by index.
// OpenAI-compatible stream clients receive tool call names and JSON arguments
// in fragments, so this lives with the shared ToolCall type.
type ToolCallAccumulator struct {
	calls map[int]ToolCall
	order []int
}

func NewToolCallAccumulator() *ToolCallAccumulator {
	return &ToolCallAccumulator{calls: make(map[int]ToolCall)}
}

func (a *ToolCallAccumulator) Add(index int, delta ToolCall) {
	if a == nil {
		return
	}
	call, ok := a.calls[index]
	if !ok {
		a.order = append(a.order, index)
		call.Type = "function"
	}
	if delta.ID != "" {
		call.ID = delta.ID
	}
	if delta.Type != "" {
		call.Type = delta.Type
	}
	call.Function.Name += delta.Function.Name
	call.Function.Arguments += delta.Function.Arguments
	a.calls[index] = call
}

func (a *ToolCallAccumulator) List() []ToolCall {
	if a == nil || len(a.order) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		call := a.calls[idx]
		if call.Type == "" {
			call.Type = "function"
		}
		if call.Function.Name == "" && call.Function.Arguments == "" {
			continue
		}
		out = append(out, call)
	}
	return out
}
