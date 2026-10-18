package agent

import (
	"encoding/json"
	"strings"
)

// Input is a user message plus per-turn options.
type Input struct {
	// Parts is the message content in order.
	Parts []Part
	// ClientID optionally identifies this message; harnesses that support
	// it echo the ID back so replies can be matched to the message.
	ClientID string
	// Delivery decides what happens when a turn is already running.
	Delivery Delivery

	// Model overrides the model from this turn on, where supported.
	// Claude cannot delay the switch until queued input starts, so it
	// refuses Model and Effort on input queued behind a running turn.
	Model string
	// Effort overrides the reasoning effort from this turn on, where
	// supported.
	Effort Effort
	// OutputSchema asks for a final answer matching this JSON Schema.
	// The structured answer is returned in TurnResult.StructuredOutput.
	// It overrides Config.OutputSchema for this turn.
	OutputSchema json.RawMessage

	// ProviderOptions carries provider-specific turn settings, such as
	// claude.TurnOptions. Send fails for a value of another provider's type.
	ProviderOptions any
}

// Delivery decides how input sent during a running turn is handled.
// Empty means DeliverQueue. Send fails for a value not listed here.
type Delivery string

// Delivery modes.
const (
	// DeliverQueue runs the input as a new turn after the running turn.
	DeliverQueue Delivery = "queue"
	// DeliverSteer injects the input into the running turn so the model
	// sees it before its next step.
	DeliverSteer Delivery = "steer"
	// DeliverInterrupt stops the running turn and then runs the input.
	DeliverInterrupt Delivery = "interrupt"
)

// PartType names a kind of input content. Send fails for a type not listed
// here.
type PartType string

// Input part types.
const (
	PartText  PartType = "text"
	PartImage PartType = "image"
	// PartFile references a file the harness should read or attach.
	PartFile PartType = "file"
	// PartSkill invokes a named skill. Codex also needs its Path.
	PartSkill PartType = "skill"
	// PartMention references a named resource: a Codex app, an OpenCode
	// agent, or "/name" text for other harnesses.
	PartMention PartType = "mention"
)

// Part is one piece of input content. Use the constructors such as
// TextPart and ImageFile rather than filling fields by hand.
type Part struct {
	Type PartType
	// Text holds the text of a text part.
	Text string
	// Data holds inline bytes of an image or file part.
	Data []byte
	// MediaType is the MIME type of Data, such as "image/png".
	MediaType string
	// Path is a local file path for image, file, skill, and mention parts.
	Path string
	// URL is a remote location for image parts.
	URL string
	// Name names a skill or mention.
	Name string
}

// Text returns input made of one text part.
func Text(text string) Input {
	return Input{Parts: []Part{TextPart(text)}}
}

// Prompt returns input made of the given parts.
func Prompt(parts ...Part) Input {
	return Input{Parts: parts}
}

// TextPart returns a text part.
func TextPart(text string) Part {
	return Part{Type: PartText, Text: text}
}

// ImageData returns an image part with inline bytes.
func ImageData(mediaType string, data []byte) Part {
	return Part{Type: PartImage, MediaType: mediaType, Data: data}
}

// ImageFile returns an image part read from a local path. The media type
// is inferred from the file extension when the harness needs it.
func ImageFile(path string) Part {
	return Part{Type: PartImage, Path: path}
}

// ImageURL returns an image part fetched from a URL.
func ImageURL(url string) Part {
	return Part{Type: PartImage, URL: url}
}

// FileRef returns a part that points the harness at a local file.
func FileRef(path string) Part {
	return Part{Type: PartFile, Path: path}
}

// SkillPart returns a part that invokes a skill by name; path is the
// skill's file, which Codex needs.
func SkillPart(name, path string) Part {
	return Part{Type: PartSkill, Name: name, Path: path}
}

// MentionPart returns a part that mentions a named resource; see
// PartMention.
func MentionPart(name, path string) Part {
	return Part{Type: PartMention, Name: name, Path: path}
}

// PlainText joins the text parts of the input with blank lines. File parts
// are rendered as "@path", and skill and mention parts as "/name".
// Providers without structured input use it.
func (in Input) PlainText() string {
	var b strings.Builder
	for _, p := range in.Parts {
		var s string
		switch p.Type {
		case PartText:
			s = p.Text
		case PartFile:
			s = "@" + p.Path
		case PartSkill, PartMention:
			s = "/" + p.Name
		default:
			continue
		}
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}
	return b.String()
}
