package datacircle

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// converseAPI is a Converse API on 127.0.0.1 for the SDK's own client: each request's body is kept, each answer is the
// next of answers.
func converseAPI(t *testing.T, answers ...string) (*bedrockruntime.Client, *[]map[string]any) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("sent %s: %v", raw, err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		answer := `{"output":{"message":{"role":"assistant","content":[{"text":"DONE"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":1}}`
		if len(bodies) <= len(answers) {
			answer = answers[len(bodies)-1]
		}
		w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	client := bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		})})
	return client, &bodies
}

func sent(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestTools(t *testing.T) {
	client, bodies := converseAPI(t)
	tools := New(nil)
	own := &types.ToolMemberToolSpec{Value: types.ToolSpecification{Name: aws.String("get_time"),
		InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(map[string]any{"type": "object"})}}}
	_, err := client.Converse(context.Background(), &bedrockruntime.ConverseInput{ModelId: aws.String("m"),
		Messages:   []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Hi"}}}},
		ToolConfig: &types.ToolConfiguration{Tools: append(tools.Tools(), own)}})
	if err != nil {
		t.Fatal(err)
	}
	// As the client sends them: the four tool specs, then the app's own.
	var specs []map[string]any
	for _, tool := range (*bodies)[0]["toolConfig"].(map[string]any)["tools"].([]any) {
		specs = append(specs, tool.(map[string]any)["toolSpec"].(map[string]any))
	}
	if len(specs) != 5 || specs[4]["name"] != "get_time" {
		t.Fatalf("sent %s", sent(specs))
	}
	for i, name := range Names {
		schema := sent(specs[i]["inputSchema"])
		if specs[i]["name"] != name || specs[i]["description"] != abouts[name] {
			t.Errorf("tool %d: %s", i, sent(specs[i]))
		}
		switch name {
		case BestPlansName, ProviderName:
			if !strings.Contains(schema, `"required":["slug"]`) {
				t.Errorf("%s requires no slug: %s", name, schema)
			}
		default:
			if strings.Contains(schema, `"required"`) {
				t.Errorf("%s requires an input: %s", name, schema)
			}
		}
		if name == SearchPlansName && !strings.Contains(schema, `"enum":["score","provider","price"]`) {
			t.Errorf("%s's sort has no enum: %s", name, schema)
		}
		if !strings.HasPrefix(schema, `{"json":{"properties":{`) || !strings.Contains(schema, `"limit":{"description":"How many`) ||
			!strings.HasSuffix(schema, `"type":"object"}}`) {
			t.Errorf("%s's input schema: %s", name, schema)
		}
	}
}

func TestCalls(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch {
		case r.URL.Path == "/apis":
			w.Write([]byte(`[{"api":"Employee profile","slug":"linkedin-profile","sheetPage":"s"},{"api":"Employee email","slug":"email-finder","plans":3}]`))
		case r.URL.Path == "/apis/email-finder/best":
			w.Write([]byte(`{"api":"Employee email","plans":[{"provider":"A","rank":1,"terms":"long"}]}`))
		case r.URL.Path == "/plans" && (r.URL.Query().Get("api") == "Bright Data" || r.URL.Query().Get("api") == "bright-data"):
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"api must be one of: Employee email"}`))
		case r.URL.Path == "/plans" && strings.Contains(r.URL.Query().Get("query"), "-"):
			w.Write([]byte(`{"total":0,"plans":[]}`))
		case r.URL.Path == "/plans":
			w.Write([]byte(`{"total":1,"plans":[{"provider":"Bright Data","rank":2}]}`))
		case r.URL.Path == "/providers/bright-data":
			w.Write([]byte(`{"provider":"Bright Data","apis":[{"api":"Employee profile","plans":[{"plan":"C"}]},{"api":"Employee email","rank":4,"plans":[{"plan":"B","rank":9},{"plan":"A","rank":4}]}]}`))
		case r.URL.Path == "/providers/down":
			w.WriteHeader(502)
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no API has the slug nope"}`))
		}
	}))
	defer server.Close()
	tools := New(&Config{BaseURL: server.URL})
	ctx := context.Background()
	for _, call := range []struct {
		name, input string
		fails       bool
		has         string
	}{
		{ListAPIsName, `{"query":"email"}`, false, `"total":1`},
		{ListAPIsName, `{}`, false, `"total":2`},
		{BestPlansName, `{"slug":"Email Finder","limit":"2"}`, false, `"provider":"A"`},
		{SearchPlansName, `{"api":"Bright Data"}`, false, `No API is named`},
		{SearchPlansName, `{"api":"Bright Data","query":"growth"}`, true, `No API is named "Bright Data". datacircle_list_apis gives every API's name and slug.`},
		{SearchPlansName, `{"api":"bright-data","query":null}`, false, `these are the plans whose name holds \"bright data\".","plans":[{"provider":"Bright Data"`},
		{SearchPlansName, `{"query":"bright-data","sort":"price","limit":null}`, false, `"plans":[{"provider":"Bright Data"`},
		{ProviderName, `{"slug":"Bright Data"}`, false, `"apis":[{"api":"Employee email","best":{"plan":"A"`},
		{ProviderName, `{"slug":"bright-data","limit":1}`, false, `"ranked":1,"total":2`},
		{ProviderName, `{"slug":"bright-data","limit":1}`, false, `{"about":"Bright Data has plans on 2 APIs on Datacircle and a rank on 1 of them (1 is the best). Its best rank: 4, on Employee email.","apis":[`},
		{BestPlansName, `{"slug":"nope"}`, true, `no API has the slug nope. datacircle_list_apis gives every API's slug. A provider's plans: datacircle_search_plans`},
		{BestPlansName, `{"provider":"Bright Data","limit":2}`, true, `Give an API's slug, like email-finder: datacircle_list_apis gives every slug. A provider's plans:`},
		{ListAPIsName, `{"query":"Bright Data"}`, false, `"note":"No API's name holds every word of \"Bright Data\". A provider's plans: datacircle_search_plans`},
		{ProviderName, `{"slug":"down"}`, true, `datacircle: 502 Bad Gateway`},
		{BestPlansName, `{"slug":"email-finder","limit":[2]}`, true, `The input is no JSON object of this tool's input schema`},
		{ProviderName, `{"slug":"nope"}`, true, `no API has the slug nope. datacircle_search_plans with words of its name as query gives each plan's provider.`},
		{ListAPIsName, ``, false, `"total":2`},
		{"get_weather", `{}`, true, `datacircle: no tool is named "get_weather"`},
	} {
		got, err := tools.Run(ctx, call.name, call.input)
		if err != nil {
			got = err.Error()
		}
		if (err != nil) != call.fails || !strings.Contains(got, call.has) {
			t.Errorf("%s(%s) = %s, %v; want it to hold %s", call.name, call.input, got, err, call.has)
		}
		if strings.Contains(got, "terms") || strings.Contains(got, "sheetPage") {
			t.Errorf("%s gave a key it drops: %s", call.name, got)
		}
	}
	got, _ := tools.Run(ctx, ListAPIsName, `{"query":"employee email"}`)
	if strings.Contains(got, "note") {
		t.Errorf("datacircle_list_apis notes a query an API's name holds every word of: %s", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := tools.Run(canceled, ListAPIsName, `{}`); err != context.Canceled {
		t.Errorf("a canceled call gave %v", err)
	}
	if want := "/apis/email-finder/best?limit=2"; paths[2] != want {
		t.Errorf("called %s, want %s", paths[2], want)
	}
	if want := "/plans?query=bright+data&sort=price"; paths[10] != want {
		t.Errorf("called %s, want %s", paths[10], want)
	}

}

// datacircleAPI answers as the API does, for an email-finder plan, an unknown slug and the list of APIs.
func datacircleAPI(t *testing.T) *Toolset {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis":
			w.Write([]byte(`[{"api":"Employee email","slug":"email-finder","plans":3}]`))
		case "/apis/email-finder/best":
			w.Write([]byte(`{"api":"Employee email","plans":[{"provider":"A","rank":1,"terms":"long"}]}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no API has the slug nope"}`))
		}
	}))
	t.Cleanup(server.Close)
	return New(&Config{BaseURL: server.URL})
}

func TestConverse(t *testing.T) {
	ctx := context.Background()
	tools := datacircleAPI(t)
	client, bodies := converseAPI(t, `{"output":{"message":{"role":"assistant","content":[{"text":"Calling."},
		{"toolUse":{"toolUseId":"t1","name":"datacircle_best_plans","input":{"slug":"email-finder"}}},
		{"toolUse":{"toolUseId":"t2","name":"datacircle_best_plans","input":{"slug":"nope"}}},
		{"toolUse":{"toolUseId":"t3","name":"get_time","input":{"zone":"UTC"}}},
		{"toolUse":{"toolUseId":"t4","name":"datacircle_list_apis","input":{}}},
		{"toolUse":{"toolUseId":"t5","name":"web_search","input":{"q":"x"}}},
		{"toolResult":{"toolUseId":"t5","content":[{"text":"found"}]}}]}},
		"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":1}}`)
	input := &bedrockruntime.ConverseInput{ModelId: aws.String("m"), ToolConfig: &types.ToolConfiguration{Tools: tools.Tools()},
		Messages: []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Go"}}}}}
	for turn := 0; turn < 2; turn++ {
		out, err := client.Converse(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		message := out.Output.(*types.ConverseOutputMemberMessage).Value
		input.Messages = append(input.Messages, message)
		var asked []string
		answer, ok := tools.Results(ctx, message, func(_ context.Context, use types.ToolUseBlock) (string, error) {
			in, _ := inputJSON(use.Input)
			asked = append(asked, *use.Name+in)
			return `{"time":"12:00"}`, nil
		})
		if turn == 1 {
			if ok {
				t.Errorf("an answer gave results: %s", sent(answer))
			}
			break
		}
		if !ok || len(asked) != 1 || asked[0] != `get_time{"zone":"UTC"}` {
			t.Fatalf("Results gave %v, asked %q", ok, asked)
		}
		input.Messages = append(input.Messages, answer)
	}
	// The SDK sends the model's message back as Bedrock gave it, an empty input as {}, then one result per tool use.
	messages := sent((*bodies)[1]["messages"])
	want := `{"content":[{"text":"Calling."},{"toolUse":{"input":{"slug":"email-finder"},"name":"datacircle_best_plans","toolUseId":"t1"}},` +
		`{"toolUse":{"input":{"slug":"nope"},"name":"datacircle_best_plans","toolUseId":"t2"}},{"toolUse":{"input":{"zone":"UTC"},"name":"get_time","toolUseId":"t3"}},` +
		`{"toolUse":{"input":{},"name":"datacircle_list_apis","toolUseId":"t4"}},{"toolUse":{"input":{"q":"x"},"name":"web_search","toolUseId":"t5"}},` +
		`{"toolResult":{"content":[{"text":"found"}],"toolUseId":"t5"}}],"role":"assistant"},` +
		`{"content":[{"toolResult":{"content":[{"text":"{\"api\":\"Employee email\",\"plans\":[{\"provider\":\"A\",\"rank\":1}]}"}],"toolUseId":"t1"}},` +
		`{"toolResult":{"content":[{"text":"{\"error\":\"no API has the slug nope. datacircle_list_apis gives every API's slug. A provider's plans: datacircle_search_plans with its name as query, like bright data, or datacircle_provider.\"}"}],"toolUseId":"t2"}},` +
		`{"toolResult":{"content":[{"text":"{\"time\":\"12:00\"}"}],"toolUseId":"t3"}},` +
		`{"toolResult":{"content":[{"text":"{\"apis\":[{\"api\":\"Employee email\",\"plans\":3,\"slug\":\"email-finder\"}],\"total\":1}"}],"toolUseId":"t4"}}],"role":"user"}]`
	if !strings.HasSuffix(messages, want) {
		t.Errorf("sent %s\nwant it to end %s", messages, want)
	}

	// With no handler of its own, a use of the app's tool is answered with an error the model reads.
	answer, _ := tools.Results(ctx, input.Messages[1], nil)
	if got := sent(answer.Content[2]); !strings.Contains(got, `"Value":"{\"error\":\"No tool is named \\\"get_time\\\" here.\"}"`) {
		t.Errorf("Results gave %s", got)
	}
	if _, ok := tools.Result(ctx, types.ToolUseBlock{Name: aws.String("get_time")}); ok {
		t.Error("Result ran another tool")
	}
}

func TestAccumulator(t *testing.T) {
	at := func(i int32) *int32 { return &i }
	delta := func(i int32, d types.ContentBlockDelta) types.ConverseStreamOutput {
		return &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: at(i), Delta: d}}
	}
	start := func(i int32, id string) types.ConverseStreamOutput {
		return &types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{ContentBlockIndex: at(i),
			Start: &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{ToolUseId: aws.String(id), Name: aws.String(BestPlansName)}}}}
	}
	piece := func(i int32, s string) types.ConverseStreamOutput {
		return delta(i, &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(s)}})
	}
	var a Accumulator
	var shown string
	for _, event := range []types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberMessageStart{Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant}},
		delta(0, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{Value: "Look "}}),
		delta(0, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{Value: "it up."}}),
		delta(0, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberSignature{Value: "sig"}}),
		&types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: at(0)}},
		delta(1, &types.ContentBlockDeltaMemberText{Value: "Call"}),
		delta(1, &types.ContentBlockDeltaMemberText{Value: "ing."}),
		delta(2, &types.ContentBlockDeltaMemberText{Value: "\n\n"}),
		start(3, "t1"), piece(3, `{"slug":`), piece(3, `"email-finder"}`),
		start(4, "t2"),
		start(5, "t3"), piece(5, ""),
		start(6, "t4"), piece(6, `{"slug":"ema`),
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
		&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{}},
	} {
		shown += a.Add(event)
	}
	message := a.Message()
	if shown != "Calling.\n\n" || a.StopReason != types.StopReasonToolUse || len(message.Content) != 6 || message.Role != types.ConversationRoleAssistant {
		t.Fatalf("shown %q, stop %q, message %s", shown, a.StopReason, sent(message))
	}
	reasoning := message.Content[0].(*types.ContentBlockMemberReasoningContent).Value.(*types.ReasoningContentBlockMemberReasoningText).Value
	if *reasoning.Text != "Look it up." || *reasoning.Signature != "sig" || message.Content[1].(*types.ContentBlockMemberText).Value != "Calling." {
		t.Errorf("message %s", sent(message))
	}
	var inputs []string
	for _, use := range ToolUses(message) {
		in, err := inputJSON(use.Input)
		inputs = append(inputs, *use.ToolUseId+in)
		if err != nil {
			t.Error(err)
		}
		// Read as an app reads a Converse input: into a struct, a map and any.
		var typed struct{ Slug string }
		var object map[string]any
		var anything any
		for _, into := range []any{&typed, &object, &anything} {
			if err := use.Input.UnmarshalSmithyDocument(into); err != nil {
				t.Errorf("%s's input into %T: %v", *use.ToolUseId, into, err)
			}
		}
		if *use.ToolUseId == "t1" && (typed.Slug != "email-finder" || object["slug"] != "email-finder" || sent(anything) != `{"slug":"email-finder"}`) {
			t.Errorf("read %+v, %v, %v", typed, object, anything)
		}
	}
	if got := strings.Join(inputs, " "); got != `t1{"slug":"email-finder"} t2{} t3{} t4{}` {
		t.Errorf("inputs %s", got)
	}
	answer, ok := datacircleAPI(t).Results(context.Background(), message, nil)
	if !ok || len(answer.Content) != 4 || answer.Role != types.ConversationRoleUser ||
		!strings.Contains(sent(answer.Content[0]), `\"provider\":\"A\"`) || !strings.Contains(sent(answer.Content[1]), `Give an API's slug`) {
		t.Errorf("Results gave %s", sent(answer))
	}

	// The SDK sends the gathered message as Bedrock takes it.
	client, bodies := converseAPI(t)
	_, err := client.Converse(context.Background(), &bedrockruntime.ConverseInput{ModelId: aws.String("m"),
		ToolConfig: &types.ToolConfiguration{Tools: New(nil).Tools()}, Messages: []types.Message{
			{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Go"}}}, message, answer}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sent((*bodies)[0]["messages"].([]any)[1]), `{"content":[{"reasoningContent":{"reasoningText":{"signature":"sig","text":"Look it up."}}},{"text":"Calling."},`+
		`{"toolUse":{"input":{"slug":"email-finder"},"name":"datacircle_best_plans","toolUseId":"t1"}},{"toolUse":{"input":{},"name":"datacircle_best_plans","toolUseId":"t2"}},`+
		`{"toolUse":{"input":{},"name":"datacircle_best_plans","toolUseId":"t3"}},{"toolUse":{"input":{},"name":"datacircle_best_plans","toolUseId":"t4"}}],"role":"assistant"}`; got != want {
		t.Errorf("sent %s\nwant %s", got, want)
	}
	if _, err := Collect(nil); err == nil {
		t.Error("Collect read no stream")
	}
}

// TestLive calls each tool once on the API itself: DATACIRCLE_LIVE=1 go test -run Live.
func TestLive(t *testing.T) {
	if os.Getenv("DATACIRCLE_LIVE") == "" {
		t.Skip("DATACIRCLE_LIVE is unset")
	}
	tools := New(nil)
	ctx := context.Background()
	for _, call := range []struct{ name, input, has string }{
		{ListAPIsName, `{"query":"email finder","limit":3}`, `"slug":"email-finder"`},
		{BestPlansName, `{"slug":"email-finder","limit":3}`, `"rank":1`},
		{SearchPlansName, `{"query":"bright data","sort":"price","limit":3}`, `"provider":"Bright Data"`},
		{ProviderName, `{"slug":"Bright Data","limit":3}`, `"about":"Bright Data has plans on`},
		{BestPlansName, `{"slug":"bright-data"}`, `"error":`},
	} {
		var in any
		json.Unmarshal([]byte(call.input), &in)
		result, ok := tools.Result(ctx, types.ToolUseBlock{ToolUseId: aws.String("live"), Name: aws.String(call.name), Input: document.NewLazyDocument(in)})
		got := ""
		if ok {
			got = result.Content[0].(*types.ToolResultContentBlockMemberText).Value
		}
		if !ok || *result.ToolUseId != "live" || !strings.Contains(got, call.has) {
			t.Errorf("%s(%s) = %s; want it to hold %s", call.name, call.input, got, call.has)
		}
		t.Logf("%s(%s) = %.300s", call.name, call.input, got)
	}
}
