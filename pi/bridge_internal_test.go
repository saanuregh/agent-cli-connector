package pi

import (
	"encoding/json"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestBridgeToolResultEncoding(t *testing.T) {
	cases := []struct {
		res  agent.ToolResult
		want string
	}{
		{agent.ToolResult{IsError: true}, `{"content":[],"isError":true}`},
		{
			agent.ToolResult{
				Content: []agent.ToolContent{
					{Type: agent.ToolContentText, Text: "hi"},
					{Type: agent.ToolContentImage, Data: []byte{1, 2}, MediaType: "image/png"},
				},
				Structured: json.RawMessage(`{"a":1}`),
			},
			`{"content":[{"text":"hi","type":"text"},{"data":"AQI=","mimeType":"image/png","type":"image"}],"isError":false,"structured":{"a":1}}`,
		},
	}
	for _, c := range cases {
		got, err := json.Marshal(bridgeToolResult(c.res))
		if err != nil || string(got) != c.want {
			t.Errorf("bridgeToolResult(%+v) = %s, %v; want %s", c.res, got, err, c.want)
		}
	}
}
