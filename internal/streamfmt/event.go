package streamfmt

// EventKind identifies a provider-neutral stream event.
type EventKind uint8

const (
	EventBoundary EventKind = iota
	EventText
	EventReasoning
	EventTool
	EventLiteral
)

// Event contains only the display-ready data needed by Renderer.
type Event struct {
	Kind EventKind
	Text string
	Name string
	Arg  string
}
