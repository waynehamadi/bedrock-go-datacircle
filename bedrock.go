package datacircle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithyjson "github.com/aws/smithy-go/document/json"
)

// Tools gives the four tools for a request's ToolConfig (types.ToolConfiguration), each a tool spec whose input schema
// is a JSON schema. Your own tools go beside them: append(tools.Tools(), yours...).
func (t *Toolset) Tools() []types.Tool {
	out := make([]types.Tool, 0, len(Names))
	for _, name := range Names {
		out = append(out, &types.ToolMemberToolSpec{Value: types.ToolSpecification{Name: ptr(name),
			Description: ptr(abouts[name]),
			InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(parameters(name))}}})
	}
	return out
}

// Result runs a tool use of the model's message, when it calls one of the four tools, into the toolResult that
// answers it: the tool's result as JSON text, or {"error": ...} with the API's words or the failure's, which the model
// reads and can call again on. Its false is a use of another tool, for the app to run.
func (t *Toolset) Result(ctx context.Context, use types.ToolUseBlock) (types.ToolResultBlock, bool) {
	name := value(use.Name)
	if !t.Handles(name) {
		return types.ToolResultBlock{}, false
	}
	input, err := inputJSON(use.Input)
	if err != nil {
		return textResult(use.ToolUseId, errorText(advice("The input is no JSON object: "+err.Error()))), true
	}
	return textResult(use.ToolUseId, t.output(ctx, name, input)), true
}

// Own runs a use of one of the app's own tools: its result as text, often JSON, or an error the model reads as
// {"error": its words}.
type Own func(ctx context.Context, use types.ToolUseBlock) (string, error)

// Results answers each tool use of the model's message, in order, in one user message of toolResult blocks, for the
// next request's Messages: the four tools' uses by Result, the others by own. Bedrock wants every tool use of a turn
// answered in the next message: with own nil, a use of another tool is answered {"error": "No tool is named ..."}.
// Its false is a message with no tool use to answer, the model's answer.
func (t *Toolset) Results(ctx context.Context, message types.Message, own Own) (types.Message, bool) {
	uses := ToolUses(message)
	if len(uses) == 0 {
		return types.Message{}, false
	}
	content := make([]types.ContentBlock, 0, len(uses))
	for _, use := range uses {
		result, ok := t.Result(ctx, use)
		if !ok {
			text, err := "", fmt.Errorf("No tool is named %q here.", value(use.Name))
			if own != nil {
				text, err = own(ctx, use)
			}
			if err != nil {
				text = errorText(err)
			}
			result = textResult(use.ToolUseId, text)
		}
		content = append(content, &types.ContentBlockMemberToolResult{Value: result})
	}
	return types.Message{Role: types.ConversationRoleUser, Content: content}, true
}

// ToolUses gives the tool uses of the model's message that the app answers, in order: each toolUse block, but one the
// message answers itself with a toolResult block (a system tool's, which Bedrock runs).
func ToolUses(message types.Message) []types.ToolUseBlock {
	answered := map[string]bool{}
	for _, block := range message.Content {
		if result, ok := block.(*types.ContentBlockMemberToolResult); ok {
			answered[value(result.Value.ToolUseId)] = true
		}
	}
	var uses []types.ToolUseBlock
	for _, block := range message.Content {
		if use, ok := block.(*types.ContentBlockMemberToolUse); ok && !answered[value(use.Value.ToolUseId)] {
			uses = append(uses, use.Value)
		}
	}
	return uses
}

// inputJSON is a tool use's input as JSON: {} when it has none.
func inputJSON(input document.Interface) (string, error) {
	if input == nil {
		return "{}", nil
	}
	raw, err := input.MarshalSmithyDocument()
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// textResult is a toolResult of one text block. A blank text is sent as {}: Bedrock refuses a blank one.
func textResult(id *string, text string) types.ToolResultBlock {
	if strings.TrimSpace(text) == "" {
		text = "{}"
	}
	return types.ToolResultBlock{ToolUseId: id, Content: []types.ToolResultContentBlock{
		&types.ToolResultContentBlockMemberText{Value: text}}}
}

// Accumulator gathers a ConverseStream's events into the model's message, as Converse gives it: its text, its
// reasoning, with the signature Bedrock wants back beside a tool use, and each tool use, whose input comes in pieces
// of JSON. Other blocks (citations, images, a system tool's) are left out. Its zero value is ready to use.
type Accumulator struct {
	// StopReason is the stream's, from its messageStop event: tool_use when the model used tools.
	StopReason types.StopReason

	blocks map[int32]*streamed
}

// streamed is one content block of the stream, by its contentBlockIndex.
type streamed struct {
	text, reasoning, input strings.Builder
	signature              string
	redacted               []byte
	use                    *types.ToolUseBlock
}

func (a *Accumulator) block(index *int32) *streamed {
	if a.blocks == nil {
		a.blocks = map[int32]*streamed{}
	}
	i := int32(0)
	if index != nil {
		i = *index
	}
	if a.blocks[i] == nil {
		a.blocks[i] = &streamed{}
	}
	return a.blocks[i]
}

// Add takes the stream's next event and gives the text it adds to the model's answer, for an app that shows the
// answer as it comes.
func (a *Accumulator) Add(event types.ConverseStreamOutput) string {
	switch e := event.(type) {
	case *types.ConverseStreamOutputMemberContentBlockStart:
		if start, ok := e.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
			a.block(e.Value.ContentBlockIndex).use = &types.ToolUseBlock{Name: start.Value.Name,
				ToolUseId: start.Value.ToolUseId}
		}
	case *types.ConverseStreamOutputMemberContentBlockDelta:
		b := a.block(e.Value.ContentBlockIndex)
		switch d := e.Value.Delta.(type) {
		case *types.ContentBlockDeltaMemberText:
			b.text.WriteString(d.Value)
			return d.Value
		case *types.ContentBlockDeltaMemberToolUse:
			b.input.WriteString(value(d.Value.Input))
		case *types.ContentBlockDeltaMemberReasoningContent:
			switch r := d.Value.(type) {
			case *types.ReasoningContentBlockDeltaMemberText:
				b.reasoning.WriteString(r.Value)
			case *types.ReasoningContentBlockDeltaMemberSignature:
				b.signature += r.Value
			case *types.ReasoningContentBlockDeltaMemberRedactedContent:
				b.redacted = append(b.redacted, r.Value...)
			}
		}
	case *types.ConverseStreamOutputMemberMessageStop:
		a.StopReason = e.Value.StopReason
	}
	return ""
}

// Message gives the model's message the events make, for the next request's Messages and for Results. A tool use's
// input that is no JSON object, like none at all, is given as {}: Bedrock takes only an object. A blank text is left
// out, as Bedrock refuses it.
func (a *Accumulator) Message() types.Message {
	indexes := make([]int32, 0, len(a.blocks))
	for i := range a.blocks {
		indexes = append(indexes, i)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	content := []types.ContentBlock{}
	for _, i := range indexes {
		b := a.blocks[i]
		switch {
		case b.use != nil:
			var input any
			if json.Unmarshal([]byte(b.input.String()), &input) != nil {
				input = nil
			}
			object, ok := input.(map[string]any)
			if !ok {
				object = map[string]any{}
			}
			use := *b.use
			use.Input = streamedInput{document.NewLazyDocument(object)}
			content = append(content, &types.ContentBlockMemberToolUse{Value: use})
		case len(b.redacted) > 0:
			content = append(content, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberRedactedContent{Value: b.redacted}})
		case b.reasoning.Len() > 0 || b.signature != "":
			reasoning := types.ReasoningTextBlock{Text: ptr(b.reasoning.String())}
			if b.signature != "" {
				reasoning.Signature = ptr(b.signature)
			}
			content = append(content, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberReasoningText{Value: reasoning}})
		case strings.TrimSpace(b.text.String()) != "":
			content = append(content, &types.ContentBlockMemberText{Value: b.text.String()})
		}
	}
	return types.Message{Role: types.ConversationRoleAssistant, Content: content}
}

// streamedInput is a tool use's input gathered from a stream: the SDK's lazy document of it, which the SDK sends as
// JSON, read as Converse's own inputs are. The lazy document's own UnmarshalSmithyDocument fails on every target.
type streamedInput struct{ document.Interface }

func (in streamedInput) UnmarshalSmithyDocument(v any) error {
	raw, err := in.MarshalSmithyDocument()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return smithyjson.NewDecoder().DecodeJSONInterface(value, v)
}

// Collect reads a ConverseStream's events to its end and gives the model's message they make, with the stream's
// error, if any. It closes the stream.
func Collect(stream *bedrockruntime.ConverseStreamEventStream) (types.Message, error) {
	if stream == nil {
		return types.Message{}, errors.New("datacircle: no stream")
	}
	var a Accumulator
	for event := range stream.Events() {
		a.Add(event)
	}
	err := stream.Close()
	if streamErr := stream.Err(); streamErr != nil {
		err = streamErr
	}
	return a.Message(), err
}

func ptr(s string) *string { return &s }

func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
