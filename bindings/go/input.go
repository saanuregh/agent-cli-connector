package agent

import (
	"encoding/base64"
	"strings"
)

// Text is input of plain text.
func Text(text string) Input { return Input{Text: text} }

// Prompt is input made of parts, such as text with images.
func Prompt(parts ...Part) Input { return Input{Parts: parts} }

// TextPart is a text part.
func TextPart(text string) Part { return Part{Type: PartTypeText, Text: text} }

// ImageData is an image part with inline data.
func ImageData(mediaType string, data []byte) Part {
	return Part{Type: PartTypeImage, MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}
}

// ImageFile is an image part read from a local file.
func ImageFile(path string) Part { return Part{Type: PartTypeImage, Path: path} }

// ImageURL is an image part fetched from a URL.
func ImageURL(url string) Part { return Part{Type: PartTypeImage, URL: url} }

// FileRef is a reference to a local file.
func FileRef(path string) Part { return Part{Type: PartTypeFile, Path: path} }

// SkillPart invokes a harness skill by name; path is the skill file when known.
func SkillPart(name, path string) Part { return Part{Type: PartTypeSkill, Name: name, Path: path} }

// MentionPart mentions a harness entity, such as an agent, by name.
func MentionPart(name, path string) Part { return Part{Type: PartTypeMention, Name: name, Path: path} }

// Text joins the text content of a tool result.
func (r ToolResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type != ToolContentTypeText && c.Type != "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(c.Text)
	}
	return b.String()
}
